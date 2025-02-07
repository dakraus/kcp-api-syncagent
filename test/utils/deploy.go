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

package utils

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/kcp-dev/logicalcluster/v3"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/scale/scheme"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func RunAgent(
	ctx context.Context,
	t *testing.T,
	name string,
	kcpKubeconfig string,
	localKubeconfig string,
	apiExportCluster logicalcluster.Path,
	apiExport string,
) context.CancelFunc {
	t.Helper()

	t.Logf("Running agent %q…", name)

	args := []string{
		"--agent-name", name,
		"--apiexport-ref", apiExport,
		"--enable-leader-election=false",
		"--kubeconfig", localKubeconfig,
		"--kcp-kubeconfig", kcpKubeconfig,
		"--namespace", "kube-system",
	}

	localCtx, cancel := context.WithCancel(ctx)

	cmd := exec.CommandContext(localCtx, "_build/api-syncagent", args...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start api-syncagent: %v", err)
	}

	cancelAndWait := func() {
		cancel()
		_ = cmd.Wait()
	}

	t.Cleanup(cancelAndWait)

	return cancelAndWait
}

func RunEnvtest(t *testing.T) (string, ctrlruntimeclient.Client, context.CancelFunc) {
	t.Helper()

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../../deploy/crd/kcp.io"},
		ErrorIfCRDPathMissing: true,
	}

	_, err := testEnv.Start()
	if err != nil {
		t.Fatalf("Failed to start envtest: %v", err)
	}

	adminKubeconfig, adminRestConfig := createEnvtestKubeconfig(t, testEnv)
	if err != nil {
		t.Fatal(err)
	}

	sc := runtime.NewScheme()
	if err := scheme.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}

	client, err := ctrlruntimeclient.New(adminRestConfig, ctrlruntimeclient.Options{
		Scheme: sc,
	})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	cancelAndWait := func() {
		_ = testEnv.Stop()
	}

	t.Cleanup(cancelAndWait)

	return adminKubeconfig, client, cancelAndWait
}

func createEnvtestKubeconfig(t *testing.T, env *envtest.Environment) (string, *rest.Config) {
	adminInfo := envtest.User{Name: "admin", Groups: []string{"system:masters"}}

	adminUser, err := env.ControlPlane.AddUser(adminInfo, nil)
	if err != nil {
		t.Fatal(err)
	}

	adminKubeconfig, err := adminUser.KubeConfig()
	if err != nil {
		t.Fatal(err)
	}

	kubeconfigFile, err := os.CreateTemp(os.TempDir(), "kubeconfig*")
	if err != nil {
		t.Fatalf("Failed to create envtest kubeconfig file: %v", err)
	}
	defer kubeconfigFile.Close()

	if _, err := kubeconfigFile.Write(adminKubeconfig); err != nil {
		t.Fatalf("Failed to write envtest kubeconfig file: %v", err)
	}

	return kubeconfigFile.Name(), adminUser.Config()
}
