#!/usr/bin/env bash

# Copyright 2025 The KCP Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

cd $(dirname $0)/..
source hack/lib.sh

BOILERPLATE_HEADER="$(realpath hack/boilerplate/generated/boilerplate.go.txt)"
SDK_MODULE="github.com/kcp-dev/api-syncagent/sdk"
APIS_PKG="$SDK_MODULE/apis"

mkdir -p _tools
export GOBIN=$(realpath _tools)

set -x

go install k8s.io/code-generator/cmd/applyconfiguration-gen
go install k8s.io/code-generator/cmd/client-gen
go install github.com/kcp-dev/code-generator/v2
go install github.com/openshift-eng/openshift-goimports
go install sigs.k8s.io/controller-tools/cmd/controller-gen

# these are types only used for testing the syncer
$GOBIN/controller-gen \
  "object:headerFile=$BOILERPLATE_HEADER" \
  paths=./internal/sync/apis/...

cd sdk
rm -rf -- applyconfiguration clientset informers listers

$GOBIN/controller-gen \
  "object:headerFile=$BOILERPLATE_HEADER" \
  paths=./apis/...

$GOBIN/applyconfiguration-gen \
  --go-header-file "$BOILERPLATE_HEADER" \
  --output-dir applyconfiguration \
  --output-pkg $SDK_MODULE/applyconfiguration \
  ./apis/...

$GOBIN/client-gen \
  --go-header-file "$BOILERPLATE_HEADER" \
  --output-dir clientset \
  --output-pkg $SDK_MODULE/clientset \
  --clientset-name versioned \
  --input-base $APIS_PKG \
  --input syncagent/v1alpha1

$GOBIN/code-generator \
  "client:headerFile=$BOILERPLATE_HEADER,apiPackagePath=$APIS_PKG,outputPackagePath=$SDK_MODULE,singleClusterClientPackagePath=$SDK_MODULE/clientset/versioned,singleClusterApplyConfigurationsPackagePath=applyconfiguration" \
  "informer:headerFile=$BOILERPLATE_HEADER,apiPackagePath=$APIS_PKG,outputPackagePath=$SDK_MODULE,singleClusterClientPackagePath=$SDK_MODULE/clientset/versioned" \
  "lister:headerFile=$BOILERPLATE_HEADER,apiPackagePath=$APIS_PKG" \
  "paths=./apis/..." \
  "output:dir=."

# Use openshift's import fixer because gimps fails to parse some of the files;
# its output is identical to how gimps would sort the imports, but it also fixes
# the misplaced go:build directives.
$GOBIN/openshift-goimports .
