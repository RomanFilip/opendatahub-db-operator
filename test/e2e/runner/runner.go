package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "github.com/opendatahub-io/opendatahub-db-operator/api/services/v1alpha1"
	modulemanager "github.com/opendatahub-io/opendatahub-db-operator/pkg/manager"
	"github.com/opendatahub-io/opendatahub-db-operator/test/support"
)

const (
	prefix              = "[e2e-run] "
	defaultTestBinary   = "/e2e/e2e-tests"
	defaultTest2JSONBin = "/usr/local/bin/test2json"
	defaultArtifactsDir = "/artifacts"
	defaultResultsDir   = "e2e-results"
	defaultGotestsumBin = "gotestsum"
	defaultHelmBin      = "helm"
	defaultChart        = "/e2e/chart"
	defaultNamespace    = "opendatahub-db-operator-e2e"
	defaultRelease      = "opendatahub-db-operator-e2e"
	// defaultCertManagerVersion is the cert-manager Helm chart version installed
	// by Setup when cert-manager is not already present on the cluster.
	defaultCertManagerVersion = "v1.17.2"
	defaultTestTimeout        = "15m"
	defaultTestPackage        = "github.com/opendatahub-io/opendatahub-db-operator/test/e2e"
	// defaultNamespaceDeleteTimeout bounds how long Teardown waits for the
	// e2e namespace to finish terminating after requesting its deletion.
	defaultNamespaceDeleteTimeout = 5 * time.Minute
)

type Result struct {
	JUnitFile string
	JSONFile  string
	ExitCode  int
}

type Runner struct {
	stdout io.Writer
	stderr io.Writer
}

func New() *Runner {
	return &Runner{stdout: os.Stdout, stderr: os.Stderr}
}

func (r *Runner) WithStdout(w io.Writer) *Runner {
	r.stdout = w
	return r
}

func (r *Runner) WithStderr(w io.Writer) *Runner {
	r.stderr = w
	return r
}

// Setup installs cert-manager (if not present) and the operator Helm chart.
// Requires ODH_E2E_OPERATOR_IMAGE and KUBECONFIG to be set by the pipeline.
func (r *Runner) Setup() int {
	helmBin := envOr("E2E_HELM_BIN", defaultHelmBin)
	chart := envOr("E2E_CHART", defaultChart)
	namespace := envOr("ODH_E2E_NAMESPACE", defaultNamespace)
	release := envOr("ODH_E2E_RELEASE", defaultRelease)
	operatorImage := os.Getenv("ODH_E2E_OPERATOR_IMAGE")
	if operatorImage == "" {
		r.logf("ODH_E2E_OPERATOR_IMAGE is required for setup")
		return 1
	}

	cli, err := r.buildClient()
	if err != nil {
		r.logf("failed to build k8s client: %v", err)
		return 1
	}

	if code := r.installCertManager(helmBin, cli); code != 0 {
		return code
	}

	args := []string{
		"install", release, chart,
		"--namespace", namespace,
		"--create-namespace",
		"--atomic",
		"--wait",
		"--timeout", "5m",
		"--set-string", "operator.image.ref=" + operatorImage,
		"--set-string", "operator.image.pullPolicy=IfNotPresent",
	}
	r.logf("running: %s %s", helmBin, strings.Join(args, " "))
	return r.exec(helmBin, args...)
}

// installCertManager installs cert-manager via its Helm OCI chart when it is
// not already present. The existence check uses the k8s client so that kubectl
// is not required in the container.
func (r *Runner) installCertManager(helmBin string, cli client.Client) int {
	ctx := context.Background()
	version := envOr("E2E_CERT_MANAGER_VERSION", defaultCertManagerVersion)

	deploy := &appsv1.Deployment{}
	err := cli.Get(ctx, client.ObjectKey{Namespace: "cert-manager", Name: "cert-manager"}, deploy)
	if err == nil {
		r.logf("cert-manager already installed, skipping")
		return 0
	}
	if !apierrors.IsNotFound(err) && !apimeta.IsNoMatchError(err) {
		r.logf("failed to check for cert-manager deployment: %v", err)
		return 1
	}

	r.logf("installing cert-manager %s via helm OCI chart", version)
	installArgs := []string{
		"upgrade", "--install", "cert-manager",
		"oci://quay.io/jetstack/charts/cert-manager",
		"--version", version,
		"--namespace", "cert-manager",
		"--create-namespace",
		"--set", "crds.enabled=true",
		"--wait",
		"--timeout", "2m",
	}
	r.logf("running: %s %s", helmBin, strings.Join(installArgs, " "))
	return r.exec(helmBin, installArgs...)
}

// Teardown removes the DatabaseService CR, Helm release, and namespace.
// K8s operations use client-go directly; helm uninstall shells out.
// Failures are accumulated so all steps run regardless of individual errors.
func (r *Runner) Teardown() int {
	helmBin := envOr("E2E_HELM_BIN", defaultHelmBin)
	namespace := envOr("ODH_E2E_NAMESPACE", defaultNamespace)
	release := envOr("ODH_E2E_RELEASE", defaultRelease)

	cli, err := r.buildClient()
	if err != nil {
		r.logf("failed to build k8s client: %v", err)
		return 1
	}

	ctx := context.Background()
	exitCode := 0

	// Delete the DatabaseService singleton via client-go.
	r.logf("deleting DatabaseService %s", servicesv1alpha1.DatabaseServiceInstanceName)
	dbs := &servicesv1alpha1.DatabaseService{
		ObjectMeta: metav1.ObjectMeta{Name: servicesv1alpha1.DatabaseServiceInstanceName},
	}
	if err := cli.Delete(ctx, dbs); err != nil && !apierrors.IsNotFound(err) && !apimeta.IsNoMatchError(err) {
		r.logf("failed to delete DatabaseService: %v", err)
		exitCode = 1
	}

	// Uninstall the Helm release.
	helmArgs := []string{"uninstall", release, "--namespace", namespace, "--ignore-not-found", "--wait", "--timeout", "5m"}
	r.logf("running: %s %s", helmBin, strings.Join(helmArgs, " "))
	if code := r.exec(helmBin, helmArgs...); code != 0 {
		r.logf("helm uninstall exited %d", code)
		if exitCode == 0 {
			exitCode = code
		}
	}

	// Delete the namespace via support.DeleteNamespace (UID-preconditioned),
	// then wait out termination so a leftover Terminating namespace cannot
	// reject the next run's setup.
	r.logf("deleting namespace %s", namespace)
	ns := &corev1.Namespace{}
	if err := cli.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
		if !apierrors.IsNotFound(err) {
			r.logf("failed to get namespace %s: %v", namespace, err)
			if exitCode == 0 {
				exitCode = 1
			}
		}
	} else if err := support.DeleteNamespace(ctx, cli, namespace, ns.UID); err != nil {
		r.logf("failed to delete namespace %s: %v", namespace, err)
		if exitCode == 0 {
			exitCode = 1
		}
	} else {
		waitTimeout := envDurationOr("E2E_NAMESPACE_DELETE_TIMEOUT", defaultNamespaceDeleteTimeout)
		waitCtx, cancelWait := context.WithTimeout(ctx, waitTimeout)
		defer cancelWait()
		r.logf("waiting for namespace %s to terminate", namespace)
		if err := support.WaitNamespaceDeletion(waitCtx, cli, namespace, ns.UID); err != nil {
			r.logf("namespace %s did not finish terminating: %v", namespace, err)
			if exitCode == 0 {
				exitCode = 1
			}
		}
	}

	return exitCode
}

// Run executes the e2e test binary via gotestsum, writing JUnit XML to the
// results directory. Pass -test.run=<pattern> to select specific tests.
func (r *Runner) Run(args []string) Result {
	testBinary := envOr("E2E_TEST_BINARY", defaultTestBinary)
	test2jsonBin := envOr("E2E_TEST2JSON_BIN", defaultTest2JSONBin)
	artifactsDir := envOr("ARTIFACTS", defaultArtifactsDir)
	resultsDir := filepath.Join(artifactsDir, envOr("E2E_RESULTS_DIR", defaultResultsDir))
	gotestsumBin := envOr("E2E_GOTESTSUM_BIN", defaultGotestsumBin)
	goTestVerbosity := envOr("GO_TEST_VERBOSITY", "testname")
	testCount := envOr("E2E_COUNT", "1")
	testTimeout := envOr("E2E_TEST_TIMEOUT", defaultTestTimeout)
	testPackage := envOr("E2E_TEST_PACKAGE", defaultTestPackage)
	junitSuiteName := envOr("E2E_JUNIT_SUITE_NAME", "relative")
	junitClassName := envOr("E2E_JUNIT_CLASS_NAME", "relative")

	junitFile := filepath.Join(resultsDir, "junit.xml")
	jsonFile := filepath.Join(resultsDir, "log.jsonl")

	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		r.logf("failed to create results dir %s: %v", resultsDir, err)
		return Result{ExitCode: 1}
	}

	testArgs := make([]string, 0, 3+len(args))
	testArgs = append(testArgs, "-test.count="+testCount, "-test.v=test2json", "-test.timeout="+testTimeout)
	testArgs = append(testArgs, args...)

	cmdArgs := make([]string, 0, 15+len(testArgs))
	cmdArgs = append(cmdArgs,
		"--raw-command",
		"--junitfile", junitFile,
		"--junitfile-testsuite-name", junitSuiteName,
		"--junitfile-testcase-classname", junitClassName,
		"--jsonfile", jsonFile,
		"--format", goTestVerbosity,
		"--",
		test2jsonBin, "-p", testPackage, "-t", testBinary,
	)
	cmdArgs = append(cmdArgs, testArgs...)

	r.logf("running: %s %s", gotestsumBin, strings.Join(cmdArgs, " "))

	cmd := exec.Command(gotestsumBin, cmdArgs...)
	cmd.Stdout = r.stdout
	cmd.Stderr = r.stderr
	cmd.Stdin = os.Stdin

	result := Result{JUnitFile: junitFile, JSONFile: jsonFile}

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			r.logf("junit: %s", junitFile)
			r.logf("jsonl: %s", jsonFile)
			return result
		}
		r.logf("failed to run gotestsum: %v", err)
		result.ExitCode = 1
		return result
	}

	r.logf("junit: %s", junitFile)
	r.logf("jsonl: %s", jsonFile)
	return result
}

// buildClient constructs a controller-runtime client from the ambient KUBECONFIG.
func (r *Runner) buildClient() (client.Client, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("getting kubeconfig: %w", err)
	}

	scheme, err := modulemanager.NewScheme()
	if err != nil {
		return nil, fmt.Errorf("building scheme: %w", err)
	}

	cli, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("building client: %w", err)
	}

	return cli, nil
}

func (r *Runner) exec(bin string, args ...string) int {
	cmd := exec.Command(bin, args...)
	cmd.Stdout = r.stdout
	cmd.Stderr = r.stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		return 1
	}
	return 0
}

func (r *Runner) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(r.stderr, prefix+format+"\n", args...)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDurationOr(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}
