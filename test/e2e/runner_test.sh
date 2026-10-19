#!/bin/sh
set -eu
repo_root=$(CDPATH='' cd -- "$(dirname "$0")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
runner_dir="$tmp/e2e"
results_dir="$tmp/results"
diagnostics_file="$results_dir/diagnostics.log"
call_log="$tmp/calls.log"
mkdir -p "$runner_dir" "$results_dir"
sed -e "s|/e2e/|$runner_dir/|g" -e "s|/artifacts/e2e-results|$results_dir|g" "$repo_root/test/e2e/runner.sh" > "$tmp/runner.sh"

cat > "$runner_dir/e2e-cleanup" <<'STUB'
#!/bin/sh
printf 'cleanup %s\n' "$*" >> "$CALL_LOG"
status=0
case "$1" in
  preflight) if [ -n "$PREFLIGHT_STATUS" ]; then status=$PREFLIGHT_STATUS; fi ;;
  prepare-namespace) if [ -n "$PREPARE_STATUS" ]; then status=$PREPARE_STATUS; fi ;;
  check-prerequisites) if [ -n "$PREREQUISITES_STATUS" ]; then status=$PREREQUISITES_STATUS; fi ;;
  authorize-teardown) if [ -n "$AUTHORIZE_STATUS" ]; then status=$AUTHORIZE_STATUS; fi ;;
  database-service) if [ -n "$DATABASE_SERVICE_STATUS" ]; then status=$DATABASE_SERVICE_STATUS; fi ;;
  namespace) if [ -n "$NAMESPACE_STATUS" ]; then status=$NAMESPACE_STATUS; fi ;;
  diagnostics) echo 'bounded diagnostics'; if [ -n "$DIAGNOSTICS_STATUS" ]; then status=$DIAGNOSTICS_STATUS; fi ;;
esac
exit "$status"
STUB
cat > "$runner_dir/helm" <<'STUB'
#!/bin/sh
printf 'helm driver=%s' "$HELM_DRIVER" >> "$CALL_LOG"
for arg do printf '<%s>' "$arg" >> "$CALL_LOG"; done
printf '\n' >> "$CALL_LOG"
status=0
case "$1" in
  upgrade) if [ -n "$HELM_UPGRADE_STATUS" ]; then status=$HELM_UPGRADE_STATUS; fi ;;
  uninstall) if [ -n "$HELM_UNINSTALL_STATUS" ]; then status=$HELM_UNINSTALL_STATUS; fi ;;
esac
exit "$status"
STUB
cat > "$runner_dir/e2e-tests" <<'STUB'
#!/bin/sh
list_next=0
selector=
for arg do
  case "$arg" in
    -test.list=*) selector=$(printf '%s' "$arg" | cut -d= -f2-) ;;
    -test.list) list_next=1 ;;
    *) if [ "$list_next" -eq 1 ]; then selector=$arg; list_next=0; fi ;;
  esac
done
if [ -n "$selector" ]; then
  case "$selector" in *'['*) echo invalid-regexp >&2; exit 2 ;; *NoSuch*) exit 0 ;; esac
fi
echo TestHelmInstalledManagerReconcilesDatabaseService
STUB
cat > "$runner_dir/gotestsum" <<'STUB'
#!/bin/sh
printf '%s\n' "$*" > "$GOTESTSUM_LOG"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --jsonfile) printf '{}\n' > "$2"; shift 2 ;;
    --junitfile) printf '<testsuite/>\n' > "$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$GOTESTSUM_STATUS" ]; then exit "$GOTESTSUM_STATUS"; fi
exit 0
STUB
cat > "$runner_dir/test2json" <<'STUB'
#!/bin/sh
exit 0
STUB
chmod +x "$runner_dir"/*
export CALL_LOG="$call_log" GOTESTSUM_LOG="$tmp/gotestsum.log"

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
contains() { case "$1" in *"$2"*) return 0 ;; *) fail "expected [$1] to contain [$2]"; return 1 ;; esac; }
assert_order() {
  first=$(grep -n -F "$1" "$call_log" | head -n 1 | cut -d: -f1 || true)
  second=$(grep -n -F "$2" "$call_log" | head -n 1 | cut -d: -f1 || true)
  if [ -z "$first" ] || [ -z "$second" ] || [ "$first" -ge "$second" ]; then fail "expected [$1] before [$2]"; fi
}
expect_status() {
  expected=$1; shift; set +e; "$@"; actual=$?; set -e
  if [ "$actual" -ne "$expected" ]; then fail "expected status $expected, got $actual"; fi
}

: > "$call_log"
if ! ODH_E2E_OPERATOR_IMAGE= ODH_E2E_PLATFORM_TYPE= ODH_E2E_PLATFORM_VERSION= sh "$tmp/runner.sh" setup; then fail 'default setup failed'; fi
contains "$(cat "$call_log")" 'cleanup preflight --name opendatahub-db-operator-e2e --release opendatahub-db-operator-e2e' || true
contains "$(cat "$call_log")" 'operator.image.ref=quay.io/opendatahub/opendatahub-db-operator:latest' || true
contains "$(cat "$call_log")" 'platform.type=OpenDataHub' || true
contains "$(cat "$call_log")" 'platform.version=>' || true
contains "$(cat "$call_log")" '<--rollback-on-failure>' || true
contains "$(cat "$call_log")" '<--labels><shiftleft.opendatahub.io/owner=rhai-db-operator>' || true
contains "$(cat "$call_log")" 'helm driver=secret' || true
assert_order 'cleanup preflight' 'cleanup prepare-namespace'
assert_order 'cleanup prepare-namespace' 'cleanup check-prerequisites'
assert_order 'cleanup check-prerequisites' 'helm driver=secret<upgrade>'

: > "$call_log"
if ! ODH_E2E_OPERATOR_IMAGE=example.test/operator:pr ODH_E2E_PLATFORM_TYPE=SelfManagedRhoai ODH_E2E_PLATFORM_VERSION=3.6.0 sh "$tmp/runner.sh" setup; then fail 'configured setup failed'; fi
contains "$(cat "$call_log")" 'operator.image.ref=example.test/operator:pr' || true
contains "$(cat "$call_log")" 'platform.type=SelfManagedRhoai' || true
contains "$(cat "$call_log")" 'platform.version=3.6.0' || true

: > "$call_log"
expect_status 9 env PREFLIGHT_STATUS=9 sh "$tmp/runner.sh" setup
if grep -q 'cleanup prepare-namespace\|helm driver=' "$call_log"; then fail 'setup mutated after preflight failure'; fi
: > "$call_log"
expect_status 8 env PREREQUISITES_STATUS=8 sh "$tmp/runner.sh" setup
if grep -q 'helm driver=' "$call_log"; then fail 'setup installed Helm release after prerequisite failure'; fi
contains "$(cat "$call_log")" 'cleanup diagnostics --name opendatahub-db-operator-e2e' || true
: > "$call_log"
expect_status 15 env HELM_UPGRADE_STATUS=15 DIAGNOSTICS_STATUS=4 sh "$tmp/runner.sh" setup
contains "$(cat "$call_log")" 'cleanup diagnostics --name opendatahub-db-operator-e2e' || true
contains "$(cat "$diagnostics_file")" 'bounded diagnostics' || true

: > "$call_log"
if ! sh "$tmp/runner.sh" teardown; then fail 'teardown failed'; fi
assert_order 'cleanup authorize-teardown' 'cleanup database-service'
assert_order 'cleanup database-service' 'helm driver=secret<uninstall>'
assert_order 'helm driver=secret<uninstall>' 'cleanup namespace'
: > "$call_log"
expect_status 0 env AUTHORIZE_STATUS=3 sh "$tmp/runner.sh" teardown
if [ "$(wc -l < "$call_log" | tr -d ' ')" -ne 1 ]; then fail 'no-op teardown performed mutations'; fi
: > "$call_log"
expect_status 7 env AUTHORIZE_STATUS=7 sh "$tmp/runner.sh" teardown
if grep -q 'database-service\|helm driver=\|cleanup namespace' "$call_log"; then fail 'teardown mutated after authorization failure'; fi
: > "$call_log"
expect_status 11 env DATABASE_SERVICE_STATUS=11 sh "$tmp/runner.sh" teardown
if grep -q 'helm driver=\|cleanup namespace' "$call_log"; then fail 'teardown continued after singleton cleanup failure'; fi
: > "$call_log"
expect_status 12 env HELM_UNINSTALL_STATUS=12 sh "$tmp/runner.sh" teardown
if grep -q 'cleanup namespace' "$call_log"; then fail 'teardown removed namespace after Helm failure'; fi

: > "$call_log"
if ! sh "$tmp/runner.sh" test -test.run=TestHelmInstalledManagerReconcilesDatabaseService; then fail 'matching test selection failed'; fi
contains "$(cat "$GOTESTSUM_LOG")" "--jsonfile $results_dir/test-events.jsonl" || true
contains "$(cat "$GOTESTSUM_LOG")" "--junitfile $results_dir/xunit_report.xml" || true
contains "$(cat "$GOTESTSUM_LOG")" '-test.v=test2json' || true
contains "$(cat "$GOTESTSUM_LOG")" '-test.timeout=10m' || true
contains "$(cat "$GOTESTSUM_LOG")" '-test.run=TestHelmInstalledManagerReconcilesDatabaseService' || true
if [ ! -s "$results_dir/test-events.jsonl" ] || [ ! -s "$results_dir/xunit_report.xml" ]; then fail 'reports were not written'; fi
for selector in '-test.run=NoSuchSmoke' '-test.run NoSuchSmoke' '-test.run=['; do
  set +e; sh "$tmp/runner.sh" test $selector >"$tmp/selection.log" 2>&1; status=$?; set -e
  if [ "$status" -eq 0 ]; then fail "empty/invalid selector succeeded: $selector"; fi
done
: > "$call_log"
expect_status 17 env GOTESTSUM_STATUS=17 DIAGNOSTICS_STATUS=4 sh "$tmp/runner.sh" test -test.run=TestHelmInstalledManagerReconcilesDatabaseService
contains "$(cat "$call_log")" 'cleanup diagnostics --name opendatahub-db-operator-e2e' || true
contains "$(cat "$diagnostics_file")" 'bounded diagnostics' || true

if [ "$failures" -ne 0 ]; then exit 1; fi
echo 'runner contract checks passed'
