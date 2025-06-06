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

/*
Package apiresourceschema contains a controller that watches for PublishedResources
and creates a matching APIResourceSchema (ARS) in kcp. The name of the generated
ARS is stored in the PublishedResource's status, so that the apiexport controller
can find and include it in the generated APIExport.

The ARS name contains a hash over the GVK that the PublishedResource is pointing
to. This is to ensure that if an PublishedResource is created, then deleted, modified
with an editor and re-applied, it won't turn into the same ARS, as we cannot simply
turn an ARS for a Pod into an ARS for a StorageClass.

There is no extra cleanup procedure in either of the clusters when a PublishedResource
is deleted. This is to prevent accidental data loss in kcp in case a service owner
accidentally (and temporarily) removed a PublishedResource.
*/
package apiresourceschema
