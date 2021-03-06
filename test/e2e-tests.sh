#!/usr/bin/env bash

# Copyright 2018 The Knative Authors
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

# This script runs the end-to-end tests against Knative Serving built from source.
# It is started by prow for each PR. For convenience, it can also be executed manually.

# If you already have a Knative cluster setup and kubectl pointing
# to it, call this script with the --run-tests arguments and it will use
# the cluster and run the tests.

# Calling this script without arguments will create a new cluster in
# project $PROJECT_ID, start knative in it, run the tests and delete the
# cluster.

source $(dirname $0)/e2e-common.sh

# Script entry point.

# Skip installing istio as an add-on.
# Temporarily increasing the cluster size for serving tests to rule out
# resource/eviction as causes of flakiness.
# Pin to 1.18 since scale test is super flakey on 1.19
initialize --skip-istio-addon --min-nodes=4 --max-nodes=4 --enable-ha --cluster-version=1.18 "$@"

# Run the tests
header "Running tests"

failed=0

# Run tests serially in the mesh and https scenarios.
parallelism=""
use_https=""
if (( MESH )); then
  parallelism="-parallel 1"
fi

if (( HTTPS )); then
  use_https="--https"
  toggle_feature autoTLS Enabled config-network
  kubectl apply -f ${E2E_YAML_DIR}/test/config/autotls/certmanager/caissuer/
  add_trap "kubectl delete -f ${E2E_YAML_DIR}/test/config/autotls/certmanager/caissuer/ --ignore-not-found" SIGKILL SIGTERM SIGQUIT
fi

# Run conformance and e2e tests.

# Currently only Istio, Contour and Kourier implement the alpha features.
alpha=""
if [[ -z "${INGRESS_CLASS}" \
  || "${INGRESS_CLASS}" == "istio.ingress.networking.knative.dev" \
  || "${INGRESS_CLASS}" == "contour.ingress.networking.knative.dev" \
  || "${INGRESS_CLASS}" == "kourier.ingress.networking.knative.dev" ]]; then
  alpha="--enable-alpha"
fi

go_test_e2e -timeout=30m \
 ./test/conformance/api/... \
 ./test/conformance/runtime/... \
 ./test/e2e \
  ${parallelism} \
  ${alpha} \
  --enable-beta \
  "--resolvabledomain=$(use_resolvable_domain)" "${use_https}" || failed=1

if (( HTTPS )); then
  kubectl delete -f ${E2E_YAML_DIR}/test/config/autotls/certmanager/caissuer/ --ignore-not-found
  toggle_feature autoTLS Disabled config-network
fi

toggle_feature tag-header-based-routing Enabled
go_test_e2e -timeout=2m ./test/e2e/tagheader || failed=1
toggle_feature tag-header-based-routing Disabled

toggle_feature multi-container Enabled
go_test_e2e -timeout=2m ./test/e2e/multicontainer || failed=1
toggle_feature multi-container Disabled

# Enable allow-zero-initial-scale before running e2e tests (for test/e2e/initial_scale_test.go).
toggle_feature allow-zero-initial-scale true config-autoscaler || fail_test
go_test_e2e -timeout=2m ./test/e2e/initscale || failed=1
toggle_feature allow-zero-initial-scale false config-autoscaler || fail_test

toggle_feature responsive-revision-gc Enabled
kubectl get cm "config-gc" -n "${SYSTEM_NAMESPACE}" -o yaml > ${TMP_DIR}/config-gc.yaml
add_trap "kubectl replace cm 'config-gc' -n ${SYSTEM_NAMESPACE} -f ${TMP_DIR}/config-gc.yaml" SIGKILL SIGTERM SIGQUIT
immediate_gc
go_test_e2e -timeout=2m ./test/e2e/gc || failed=1
kubectl replace cm "config-gc" -n ${SYSTEM_NAMESPACE} -f ${TMP_DIR}/config-gc.yaml
toggle_feature responsive-revision-gc Disabled


# Run scale tests.
# Note that we use a very high -parallel because each ksvc is run as its own
# sub-test. If this is not larger than the maximum scale tested then the test
# simply cannot pass.
go_test_e2e -timeout=20m -parallel=300 ./test/scale || failed=1

# Run HA tests separately as they're stopping core Knative Serving pods.
# Define short -spoofinterval to ensure frequent probing while stopping pods.
go_test_e2e -timeout=25m -failfast -parallel=1 ./test/ha \
  ${alpha} \
  --enable-beta \
  -replicas="${REPLICAS:-1}" \
  -buckets="${BUCKETS:-1}" \
  -spoofinterval="10ms" || failed=1

(( failed )) && fail_test

# Remove the kail log file if the test flow passes.
# This is for preventing too many large log files to be uploaded to GCS in CI.
rm "${ARTIFACTS}/k8s.log-$(basename "${E2E_SCRIPT}").txt"
success
