/*
Copyright 2025 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sync

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/kcp-dev/api-syncagent/internal/mutation"
	syncagentv1alpha1 "github.com/kcp-dev/api-syncagent/sdk/apis/syncagent/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

func (s *ResourceSyncer) processRelatedResources(log *zap.SugaredLogger, stateStore ObjectStateStore, remote, local syncSide) (requeue bool, err error) {
	for _, relatedResource := range s.pubRes.Spec.Related {
		requeue, err := s.processRelatedResource(log.With("identifier", relatedResource.Identifier), stateStore, remote, local, relatedResource)
		if err != nil {
			return false, fmt.Errorf("failed to process related resource %s: %w", relatedResource.Identifier, err)
		}

		if requeue {
			return true, nil
		}
	}

	return false, nil
}

type relatedObjectAnnotation struct {
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

func (s *ResourceSyncer) processRelatedResource(log *zap.SugaredLogger, stateStore ObjectStateStore, remote, local syncSide, relRes syncagentv1alpha1.RelatedResourceSpec) (requeue bool, err error) {
	// decide what direction to sync (local->remote vs. remote->local)
	var (
		origin syncSide
		dest   syncSide
	)

	if relRes.Origin == "service" {
		origin = local
		dest = remote
	} else {
		origin = remote
		dest = local
	}

	// find the all objects on the origin side that match the given criteria
	resolvedObjects, err := resolveRelatedResourceObjects(origin, dest, relRes)
	if err != nil {
		return false, fmt.Errorf("failed to get resolve origin objects: %w", err)
	}

	// no objects were found yet, that's okay
	if len(resolvedObjects) == 0 {
		return false, nil
	}

	slices.SortStableFunc(resolvedObjects, func(a, b resolvedObject) int {
		aKey := ctrlruntimeclient.ObjectKeyFromObject(a.original).String()
		bKey := ctrlruntimeclient.ObjectKeyFromObject(b.original).String()

		return strings.Compare(aKey, bKey)
	})

	// Synchronize objects the same way the parent object was synchronized.
	for idx, resolved := range resolvedObjects {
		destObject := &unstructured.Unstructured{}
		destObject.SetAPIVersion("v1") // we only support ConfigMaps and Secrets, both are in core/v1
		destObject.SetKind(relRes.Kind)

		if err = dest.client.Get(dest.ctx, resolved.destination, destObject); err != nil {
			destObject = nil
		}

		sourceSide := syncSide{
			ctx:         origin.ctx,
			clusterName: origin.clusterName,
			client:      origin.client,
			object:      resolved.original,
		}

		destSide := syncSide{
			ctx:         dest.ctx,
			clusterName: dest.clusterName,
			client:      dest.client,
			object:      destObject,
		}

		syncer := objectSyncer{
			// Related objects within kcp are not labelled with the agent name because it's unnecessary.
			// agentName: "",
			// use the same state store as we used for the main resource, to keep everything contained
			// in one place, on the service cluster side
			stateStore: stateStore,
			// how to create a new destination object
			destCreator: func(source *unstructured.Unstructured) *unstructured.Unstructured {
				dest := source.DeepCopy()
				dest.SetName(resolved.destination.Name)
				dest.SetNamespace(resolved.destination.Namespace)

				return dest
			},
			// ConfigMaps and Secrets have no subresources
			subresources: nil,
			// only sync the status back if the object originates in kcp,
			// as the service side should never have to rely on new status infos coming
			// from the kcp side
			syncStatusBack: relRes.Origin == "kcp",
			// if the origin is on the remote side, we want to add a finalizer to make
			// sure we can clean up properly
			blockSourceDeletion: relRes.Origin == "kcp",
			// apply mutation rules configured for the related resource
			mutator: mutation.NewMutator(relRes.Mutation),
			// we never want to store sync-related metadata inside kcp
			metadataOnDestination: false,
		}

		req, err := syncer.Sync(log, sourceSide, destSide)
		if err != nil {
			return false, fmt.Errorf("failed to sync related object: %w", err)
		}

		// Updating a related object should not immediately trigger a requeue,
		// but only after all related objects are done. This is purely to not perform
		// too many unnecessary requeues.
		requeue = requeue || req

		// now that the related object was successfully synced, we can remember its details on the
		// main object
		if relRes.Origin == "service" {
			// TODO: Improve this logic, the added index is just a hack until we find a better solution
			// to let the user know about the related object (this annotation is not relevant for the
			// syncing logic, it's purely for the end-user).
			annotation := fmt.Sprintf("%s%s.%d", relatedObjectAnnotationPrefix, relRes.Identifier, idx)

			value, err := json.Marshal(relatedObjectAnnotation{
				Namespace:  resolved.destination.Namespace,
				Name:       resolved.destination.Name,
				APIVersion: "v1", // we only support ConfigMaps and Secrets
				Kind:       relRes.Kind,
			})
			if err != nil {
				return false, fmt.Errorf("failed to encode related object annotation: %w", err)
			}

			annotations := remote.object.GetAnnotations()
			existing := annotations[annotation]

			if existing != string(value) {
				oldState := remote.object.DeepCopy()

				annotations[annotation] = string(value)
				remote.object.SetAnnotations(annotations)

				log.Debug("Remembering related object in main object…")
				if err := remote.client.Patch(remote.ctx, remote.object, ctrlruntimeclient.MergeFrom(oldState)); err != nil {
					return false, fmt.Errorf("failed to update related data in remote object: %w", err)
				}

				// requeue (since this updated the main object, we do actually want to
				// requeue immediately because successive patches would fail anyway)
				return true, nil
			}
		}
	}

	return requeue, nil
}

// resolvedObject is the result of following the configuration of a related resources. It contains
// the original object (on the origin side of the related resource) and the target name to be used
// on the destination side of the sync.
type resolvedObject struct {
	original    *unstructured.Unstructured
	destination types.NamespacedName
}

func resolveRelatedResourceObjects(relatedOrigin, relatedDest syncSide, relRes syncagentv1alpha1.RelatedResourceSpec) ([]resolvedObject, error) {
	// resolving the originNamespace first allows us to scope down any .List() calls later
	originNamespace := relatedOrigin.object.GetNamespace()
	destNamespace := relatedDest.object.GetNamespace()

	namespaceMap := map[string]string{
		originNamespace: destNamespace,
	}

	if nsSpec := relRes.Object.Namespace; nsSpec != nil {
		var err error
		namespaceMap, err = resolveRelatedResourceOriginNamespaces(relatedOrigin, relatedDest, *nsSpec)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve namespace: %w", err)
		}

		if len(namespaceMap) == 0 {
			return nil, nil
		}
	} else if originNamespace == "" {
		return nil, errors.New("primary object is cluster-scoped and no source namespace configuration was provided")
	} else if destNamespace == "" {
		return nil, errors.New("primary object copy is cluster-scoped and no source namespace configuration was provided")
	}

	// At this point we know all the namespaces in which can look for related objects.
	// For all but the label selector-based specs, this map will have exactly 1 element, otherwise
	// more. Empty maps are not possible at this point.
	// The namespace map contains a mapping from origin side to destination side.
	// Armed with this, we can now resolve the object names and thereby find all objects that match
	// this related resource configuration. Again, for label selectors this can be multiple,
	// otherwise at most 1.

	objects, err := resolveRelatedResourceObjectsInNamespaces(relatedOrigin, relatedDest, relRes, relRes.Object.RelatedResourceObjectSpec, namespaceMap)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve objects: %w", err)
	}

	return objects, nil
}

func resolveRelatedResourceOriginNamespaces(relatedOrigin, relatedDest syncSide, spec syncagentv1alpha1.RelatedResourceObjectSpec) (map[string]string, error) {
	switch {
	case spec.Reference != nil:
		originNamespace, err := resolveObjectReference(relatedOrigin.object, *spec.Reference)
		if err != nil {
			return nil, err
		}

		if originNamespace == "" {
			return nil, nil
		}

		destNamespace, err := resolveObjectReference(relatedDest.object, *spec.Reference)
		if err != nil {
			return nil, err
		}

		if destNamespace == "" {
			return nil, nil
		}

		return map[string]string{
			originNamespace: destNamespace,
		}, nil

	case spec.Selector != nil:
		namespaces := &corev1.NamespaceList{}

		selector, err := metav1.LabelSelectorAsSelector(&spec.Selector.LabelSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid selector configured: %w", err)
		}

		opts := &ctrlruntimeclient.ListOptions{
			LabelSelector: selector,
		}

		if err := relatedOrigin.client.List(relatedOrigin.ctx, namespaces, opts); err != nil {
			return nil, fmt.Errorf("failed to evaluate label selector: %w", err)
		}

		namespaceMap := map[string]string{}
		for _, namespace := range namespaces.Items {
			name := namespace.Name

			destinationName, err := applyRewrites(relatedOrigin, relatedDest, name, spec.Selector.Rewrite)
			if err != nil {
				return nil, fmt.Errorf("failed to rewrite origin namespace: %w", err)
			}

			namespaceMap[name] = destinationName
		}

		return namespaceMap, nil

	case spec.Template != nil:
		originValue, destValue, err := applyTemplateBothSides(relatedOrigin, relatedDest, *spec.Template)
		if err != nil {
			return nil, fmt.Errorf("failed to apply template: %w", err)
		}

		if originValue == "" || destValue == "" {
			return nil, nil
		}

		return map[string]string{
			originValue: destValue,
		}, nil

	default:
		return nil, errors.New("invalid sourceSpec: no mechanism configured")
	}
}

func resolveRelatedResourceObjectsInNamespaces(relatedOrigin, relatedDest syncSide, relRes syncagentv1alpha1.RelatedResourceSpec, spec syncagentv1alpha1.RelatedResourceObjectSpec, namespaceMap map[string]string) ([]resolvedObject, error) {
	result := []resolvedObject{}

	for originNamespace, destNamespace := range namespaceMap {
		nameMap, err := resolveRelatedResourceObjectsInNamespace(relatedOrigin, relatedDest, relRes, spec, originNamespace)
		if err != nil {
			return nil, fmt.Errorf("failed to find objects on origin side: %w", err)
		}

		for originName, destName := range nameMap {
			originObj := &unstructured.Unstructured{}
			originObj.SetAPIVersion("v1") // we only support ConfigMaps and Secrets, both are in core/v1
			originObj.SetKind(relRes.Kind)

			err = relatedOrigin.client.Get(relatedOrigin.ctx, types.NamespacedName{Name: originName, Namespace: originNamespace}, originObj)
			if err != nil {
				// this should rarely happen, only if an object was deleted in between the .List() call
				// above and the .Get() call here.
				if apierrors.IsNotFound(err) {
					continue
				}

				return nil, fmt.Errorf("failed to get origin object: %w", err)
			}

			result = append(result, resolvedObject{
				original: originObj,
				destination: types.NamespacedName{
					Namespace: destNamespace,
					Name:      destName,
				},
			})
		}
	}

	return result, nil
}

func resolveRelatedResourceObjectsInNamespace(relatedOrigin, relatedDest syncSide, relRes syncagentv1alpha1.RelatedResourceSpec, spec syncagentv1alpha1.RelatedResourceObjectSpec, namespace string) (map[string]string, error) {
	switch {
	case spec.Reference != nil:
		originName, err := resolveObjectReference(relatedOrigin.object, *spec.Reference)
		if err != nil {
			return nil, err
		}

		if originName == "" {
			return nil, nil
		}

		destName, err := resolveObjectReference(relatedDest.object, *spec.Reference)
		if err != nil {
			return nil, err
		}

		if destName == "" {
			return nil, nil
		}

		return map[string]string{
			originName: destName,
		}, nil

	case spec.Selector != nil:
		originObjects := &unstructured.UnstructuredList{}
		originObjects.SetAPIVersion("v1") // we only support ConfigMaps and Secrets, both are in core/v1
		originObjects.SetKind(relRes.Kind)

		selector, err := metav1.LabelSelectorAsSelector(&spec.Selector.LabelSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid selector configured: %w", err)
		}

		opts := &ctrlruntimeclient.ListOptions{
			LabelSelector: selector,
			Namespace:     namespace,
		}

		if err := relatedOrigin.client.List(relatedOrigin.ctx, originObjects, opts); err != nil {
			return nil, fmt.Errorf("failed to select origin objects based on label selector: %w", err)
		}

		nameMap := map[string]string{}
		for _, originObject := range originObjects.Items {
			name := originObject.GetName()

			destinationName, err := applyRewrites(relatedOrigin, relatedDest, name, spec.Selector.Rewrite)
			if err != nil {
				return nil, fmt.Errorf("failed to rewrite origin name: %w", err)
			}

			nameMap[name] = destinationName
		}

		return nameMap, nil

	case spec.Template != nil:
		originValue, destValue, err := applyTemplateBothSides(relatedOrigin, relatedDest, *spec.Template)
		if err != nil {
			return nil, fmt.Errorf("failed to apply template: %w", err)
		}

		if originValue == "" || destValue == "" {
			return nil, nil
		}

		return map[string]string{
			originValue: destValue,
		}, nil

	default:
		return nil, errors.New("invalid objectSpec: no mechanism configured")
	}
}

func resolveObjectReference(object *unstructured.Unstructured, ref syncagentv1alpha1.RelatedResourceObjectReference) (string, error) {
	data, err := object.MarshalJSON()
	if err != nil {
		return "", err
	}

	return resolveReference(data, ref)
}

func resolveReference(jsonData []byte, ref syncagentv1alpha1.RelatedResourceObjectReference) (string, error) {
	gval := gjson.Get(string(jsonData), ref.Path)
	if !gval.Exists() {
		return "", fmt.Errorf("cannot find %s in document", ref.Path)
	}

	// this does apply some coalescing, like turning numbers into strings
	strVal := gval.String()

	if re := ref.Regex; re != nil {
		var err error

		strVal, err = applyRegularExpression(strVal, *re)
		if err != nil {
			return "", err
		}
	}

	return strVal, nil
}

func applyRewrites(relatedOrigin, relatedDest syncSide, value string, rewrite syncagentv1alpha1.RelatedResourceSelectorRewrite) (string, error) {
	switch {
	case rewrite.Regex != nil:
		return applyRegularExpression(value, *rewrite.Regex)
	case rewrite.Template != nil:
		return applyTemplate(relatedOrigin, relatedDest, *rewrite.Template, value)
	default:
		return "", errors.New("invalid rewrite: no mechanism configured")
	}
}

func applyRegularExpression(value string, re syncagentv1alpha1.RegularExpression) (string, error) {
	if re.Pattern == "" {
		return re.Replacement, nil
	}

	expr, err := regexp.Compile(re.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid pattern %q: %w", re.Pattern, err)
	}

	return expr.ReplaceAllString(value, re.Replacement), nil
}

func applyTemplate(relatedOrigin, relatedDest syncSide, tpl syncagentv1alpha1.TemplateExpression, value string) (string, error) {
	return "", errors.New("not yet implemented")
}

func applyTemplateBothSides(relatedOrigin, relatedDest syncSide, tpl syncagentv1alpha1.TemplateExpression) (originValue, destValue string, err error) {
	return "", "", errors.New("not yet implemented")
}
