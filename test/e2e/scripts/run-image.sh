#!/usr/bin/env bash
set -euo pipefail

: "${E2E_CONTEXT:?set E2E_CONTEXT to a dedicated test cluster context}"
: "${E2E_TEST_PATTERN:?set E2E_TEST_PATTERN to select tests}"

temporary_dir=$(mktemp -d)
cleanup() {
    result=$?
    trap - EXIT
    if [[ ${setup_started:-0} == 1 ]]; then
        cleanup_result=0
        run_image teardown || cleanup_result=$?
        if [[ $result == 0 ]]; then result=$cleanup_result; fi
    fi
    rm -rf "$temporary_dir"
    exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Flatten certificates and select the context without changing the host config.
"$KUBECTL" --context "$E2E_CONTEXT" config view --minify --flatten --raw > "$temporary_dir/config"
kubectl=("$KUBECTL" --kubeconfig "$temporary_dir/config")
credential_plugin=$("${kubectl[@]}" config view -o 'jsonpath={.users[0].user.exec.command}{.users[0].user.auth-provider.name}')
if [[ -n $credential_plugin ]]; then
    echo 'The image needs a kubeconfig with a token or client certificate; host credential plugins are unavailable inside it.' >&2
    exit 1
fi
"${kubectl[@]}" get --raw=/readyz > /dev/null

# The runner teardown deletes these resources, so refuse an existing installation.
existing_namespace=$("${kubectl[@]}" get namespace "$ODH_E2E_NAMESPACE" --ignore-not-found -o name)
if [[ -n $existing_namespace ]]; then
    echo "Refusing to use existing namespace $ODH_E2E_NAMESPACE" >&2
    exit 1
fi
existing_crd=$("${kubectl[@]}" get crd databaseservices.services.platform.opendatahub.io --ignore-not-found -o name)
if [[ -n $existing_crd ]]; then
    existing_service=$("${kubectl[@]}" get databaseservice default-db-operator --ignore-not-found -o name)
    if [[ -n $existing_service ]]; then
        echo 'Refusing to use a cluster with an existing DatabaseService/default-db-operator' >&2
        exit 1
    fi
fi

mkdir -p "$E2E_ARTIFACTS"
artifacts_dir=$(cd "$E2E_ARTIFACTS" && pwd)
runtime_args=(--user "$(id -u):$(id -g)")
if [[ ${CONTAINER_TOOL##*/} == podman ]]; then
    runtime_args+=(--userns=keep-id)
fi
run_image() {
    "$CONTAINER_TOOL" run --rm --network host "${runtime_args[@]}" \
        --volume "$temporary_dir/config:/e2e/kubeconfig:ro,Z" \
        --volume "$artifacts_dir:/artifacts:Z" \
        --env KUBECONFIG=/e2e/kubeconfig \
        --env HELM_CACHE_HOME=/tmp/helm/cache \
        --env HELM_CONFIG_HOME=/tmp/helm/config \
        --env HELM_DATA_HOME=/tmp/helm/data \
        --env ODH_E2E_OPERATOR_IMAGE --env ODH_E2E_NAMESPACE --env ODH_E2E_RELEASE \
        "$E2E_IMG" "$@"
}

echo "Running $E2E_IMG against context $E2E_CONTEXT; reports: $artifacts_dir"
setup_started=1
run_image setup
run_image "-test.run=$E2E_TEST_PATTERN"
