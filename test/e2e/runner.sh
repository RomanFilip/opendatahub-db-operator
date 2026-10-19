#!/bin/sh
set -eu

export HELM_DRIVER=secret
export ODH_E2E_NAMESPACE="${ODH_E2E_NAMESPACE:-opendatahub-db-operator-e2e}"
export ODH_E2E_RELEASE="${ODH_E2E_RELEASE:-opendatahub-db-operator-e2e}"
export ODH_E2E_OPERATOR_IMAGE="${ODH_E2E_OPERATOR_IMAGE:-quay.io/opendatahub/opendatahub-db-operator:latest}"
export ODH_E2E_PLATFORM_TYPE="${ODH_E2E_PLATFORM_TYPE:-OpenDataHub}"
export ODH_E2E_PLATFORM_VERSION="${ODH_E2E_PLATFORM_VERSION-}"
RESULTS_DIR=/artifacts/e2e-results
DIAGNOSTICS_FILE="${RESULTS_DIR}/diagnostics.log"

capture_diagnostics() {
  if ! mkdir -p "${RESULTS_DIR}"; then
    echo "could not create diagnostics directory ${RESULTS_DIR}" >&2
    return 0
  fi
  if /e2e/e2e-cleanup diagnostics --name "${ODH_E2E_NAMESPACE}" >"${DIAGNOSTICS_FILE}" 2>&1; then
    cat "${DIAGNOSTICS_FILE}" || true
  else
    diagnostics_status=$?
    printf 'diagnostic collection failed with status %s\n' "${diagnostics_status}" >>"${DIAGNOSTICS_FILE}" || true
    cat "${DIAGNOSTICS_FILE}" || true
  fi
  return 0
}

mode=test
case "${1:-}" in
  setup|test|teardown)
    mode="$1"
    shift
    ;;
esac

case "${mode}" in
  setup)
    /e2e/e2e-cleanup preflight --name "${ODH_E2E_NAMESPACE}" --release "${ODH_E2E_RELEASE}"
    /e2e/e2e-cleanup prepare-namespace --name "${ODH_E2E_NAMESPACE}"
    if /e2e/e2e-cleanup check-prerequisites --name "${ODH_E2E_NAMESPACE}"; then
      :
    else
      status=$?
      capture_diagnostics
      exit "${status}"
    fi
    if /e2e/helm upgrade --install "${ODH_E2E_RELEASE}" /e2e/chart \
      --namespace "${ODH_E2E_NAMESPACE}" --rollback-on-failure --wait --timeout 5m \
      --labels shiftleft.opendatahub.io/owner=rhai-db-operator \
      --set-string "operator.image.ref=${ODH_E2E_OPERATOR_IMAGE}" \
      --set-string operator.image.pullPolicy=Always \
      --set-string "platform.type=${ODH_E2E_PLATFORM_TYPE}" \
      --set-string "platform.version=${ODH_E2E_PLATFORM_VERSION}"; then
      exit 0
    else
      status=$?
      capture_diagnostics
      exit "${status}"
    fi
    ;;
  teardown)
    if /e2e/e2e-cleanup authorize-teardown --name "${ODH_E2E_NAMESPACE}" --release "${ODH_E2E_RELEASE}"; then
      :
    else
      status=$?
      if [ "${status}" -eq 3 ]; then
        exit 0
      fi
      exit "${status}"
    fi
    if /e2e/e2e-cleanup database-service; then
      :
    else
      status=$?
      capture_diagnostics
      exit "${status}"
    fi
    if /e2e/helm uninstall "${ODH_E2E_RELEASE}" --namespace "${ODH_E2E_NAMESPACE}" \
      --ignore-not-found --wait --timeout 5m; then
      :
    else
      status=$?
      capture_diagnostics
      exit "${status}"
    fi
    if /e2e/e2e-cleanup namespace --name "${ODH_E2E_NAMESPACE}"; then
      exit 0
    else
      status=$?
      capture_diagnostics
      exit "${status}"
    fi
    ;;
  test)
    mkdir -p "${RESULTS_DIR}"
    has_run_selector=false
    selector=""
    expecting_selector=false
    for arg do
      if [ "${expecting_selector}" = true ]; then
        selector="${arg}"
        expecting_selector=false
        continue
      fi
      case "${arg}" in
        -test.run)
          has_run_selector=true
          expecting_selector=true
          ;;
        -test.run=*)
          has_run_selector=true
          selector="${arg#-test.run=}"
          ;;
      esac
    done
    if [ "${expecting_selector}" = true ]; then
      echo "-test.run requires a regular expression" >&2
      exit 1
    fi
    if [ "${has_run_selector}" = true ]; then
      if matching_tests=$(/e2e/e2e-tests "-test.list=${selector}"); then
        :
      else
        status=$?
        echo "test selection is invalid" >&2
        exit "${status}"
      fi
      match_count=$(printf '%s\n' "${matching_tests}" | awk 'NF { count++ } END { print count + 0 }')
      if [ "${match_count}" -eq 0 ]; then
        echo "test selection matched no tests" >&2
        exit 1
      fi
    fi
    if /e2e/gotestsum --raw-command --format standard-verbose \
      --jsonfile "${RESULTS_DIR}/test-events.jsonl" \
      --junitfile "${RESULTS_DIR}/xunit_report.xml" -- \
      /e2e/test2json -t /e2e/e2e-tests "$@" -test.v=test2json -test.timeout=10m; then
      exit 0
    else
      status=$?
      capture_diagnostics
      exit "${status}"
    fi
    ;;
esac
