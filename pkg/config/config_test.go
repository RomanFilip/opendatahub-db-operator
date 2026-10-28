/*
Copyright 2026.

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

package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	. "github.com/onsi/gomega"

	"github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
)

func TestLoad_Defaults(t *testing.T) {
	g := NewWithT(t)

	cfg, err := config.Load()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.PlatformType).To(Equal(config.DefaultPlatformType))
	g.Expect(cfg.PlatformVersion.String()).To(Equal("0.0.0"))
	g.Expect(cfg.OperatorNamespace).To(Equal(config.DefaultOperatorNS))
	g.Expect(cfg.PostgresImage).To(Equal(config.DefaultInternalPostgresImage))
	g.Expect(cfg.DefaultProviderName).To(Equal(config.DefaultProviderName))
}

func TestLoad_DefaultProviderName(t *testing.T) {
	g := NewWithT(t)
	cfg, err := config.Load(config.WithFS(fstest.MapFS{
		config.KeyDefaultProviderName: {Data: []byte("team-db")},
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.DefaultProviderName).To(Equal("team-db"))

	_, err = config.Load(config.WithFS(fstest.MapFS{
		config.KeyDefaultProviderName: {Data: []byte("invalid.name")},
	}))
	g.Expect(err).To(HaveOccurred())
}

func TestLoad_PostgresImage(t *testing.T) {
	g := NewWithT(t)

	cfg, err := config.Load(config.WithFS(fstest.MapFS{
		"postgres-image": {Data: []byte("registry.example.test/rhel9/postgresql-16")},
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.PostgresImage).To(Equal("registry.example.test/rhel9/postgresql-16"))
}

func TestLoad_WebhookDefaults(t *testing.T) {
	g := NewWithT(t)

	cfg, err := config.Load()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.Controller.Webhook.Enabled).To(BeTrue())
	g.Expect(cfg.Controller.Webhook.Port).To(Equal(9443))
	g.Expect(cfg.Controller.Webhook.CertDir).To(Equal("/tmp/k8s-webhook-server/serving-certs"))
}

func TestLoad_EmptyPostgresImageFallsBackToDefault(t *testing.T) {
	g := NewWithT(t)

	cfg, err := config.Load(config.WithFS(fstest.MapFS{
		"postgres-image": {Data: []byte("   ")},
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.PostgresImage).To(Equal(config.DefaultInternalPostgresImage))
}

func TestLoad_ParsesPlatformVersion(t *testing.T) {
	g := NewWithT(t)

	cfg, err := config.Load(config.WithFS(fstest.MapFS{
		config.KeyPlatformVersion: {Data: []byte("3.5.0")},
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.PlatformVersion.String()).To(Equal("3.5.0"))
}

func TestLoad_EmptyPlatformVersionIsZero(t *testing.T) {
	g := NewWithT(t)

	cfg, err := config.Load(config.WithFS(fstest.MapFS{
		config.KeyPlatformVersion: {Data: []byte("")},
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.PlatformVersion.String()).To(Equal("0.0.0"))
}

func TestLoad_InvalidPlatformVersionReturnsError(t *testing.T) {
	g := NewWithT(t)

	_, err := config.Load(config.WithFS(fstest.MapFS{
		config.KeyPlatformVersion: {Data: []byte("not-a-version")},
	}))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("not-a-version"))
}

func TestLoad_PlatformType(t *testing.T) {
	g := NewWithT(t)

	cfg, err := config.Load(config.WithFS(fstest.MapFS{
		config.KeyPlatformType: {Data: []byte(config.PlatformTypeSelfManagedRhoai)},
	}))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.PlatformType).To(Equal(config.PlatformTypeSelfManagedRhoai))
}

func TestOperatorConfigMapName_MatchesPlatformHandshake(t *testing.T) {
	g := NewWithT(t)
	// ODH injectPlatformConfig writes odh-<moduleName>-config; module name is
	// databaseservice. Chartgen must emit this exact name so platformVersion
	// merges into the ConfigMap the manager mounts.
	g.Expect(config.OperatorConfigMapName).To(Equal("odh-databaseservice-config"))
}

func TestComponentRelease(t *testing.T) {
	g := NewWithT(t)

	cfg, err := config.Load(config.WithFS(fstest.MapFS{
		config.KeyPlatformVersion: {Data: []byte("2.1.0")},
	}))
	g.Expect(err).NotTo(HaveOccurred())

	rel := cfg.ComponentRelease()
	g.Expect(rel.Name).To(Equal(config.ReleasePlatform))
	g.Expect(rel.Version).To(Equal("2.1.0"))
}

func TestPlatformRelease(t *testing.T) {
	g := NewWithT(t)

	cfg, err := config.Load(config.WithFS(fstest.MapFS{
		config.KeyPlatformType:    {Data: []byte(config.DefaultPlatformType)},
		config.KeyPlatformVersion: {Data: []byte("2.1.0")},
	}))
	g.Expect(err).NotTo(HaveOccurred())

	rel := cfg.PlatformRelease()
	g.Expect(string(rel.Name)).To(Equal(config.DefaultPlatformType))
	g.Expect(rel.Version.String()).To(Equal("2.1.0"))
}

func TestComponentRelease_EmptyVersion(t *testing.T) {
	g := NewWithT(t)

	cfg, err := config.Load()
	g.Expect(err).NotTo(HaveOccurred())

	// Zero OperatorVersion serialises as "0.0.0"
	rel := cfg.ComponentRelease()
	g.Expect(rel.Name).To(Equal(config.ReleasePlatform))
	g.Expect(rel.Version).To(Equal("0.0.0"))
}

// DatabaseService's retry-interval config key must always be config-driven,
// never a hardcoded literal in controller code, so its three-layer
// precedence (compiled default -> ConfigMap -> env var) is exercised
// explicitly, along with proof that it is independently overridable.

func TestLoad_DatabaseServiceRetryDefault(t *testing.T) {
	g := NewWithT(t)

	cfg, err := config.Load()
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(cfg.DatabaseService.RetryInterval).To(Equal(config.DefaultRetryInterval))
}

func TestLoad_DatabaseServiceRetry_ConfigMapOverride(t *testing.T) {
	g := NewWithT(t)

	cfg, err := config.Load(config.WithFS(fstest.MapFS{
		config.KeyDatabaseServiceRetryInterval: {Data: []byte("7m")},
	}))
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(cfg.DatabaseService.RetryInterval).To(Equal(7 * time.Minute))
}

func TestLoad_DatabaseServiceRetry_EnvOverridesConfigMap(t *testing.T) {
	g := NewWithT(t)

	t.Setenv("ODH_MODULE_OPERATOR_DATABASESERVICE_RETRY_INTERVAL", "90s")

	cfg, err := config.Load(config.WithFS(fstest.MapFS{
		// ConfigMap sets a different value than the env var above -- env must win.
		config.KeyDatabaseServiceRetryInterval: {Data: []byte("3m")},
	}))
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(cfg.DatabaseService.RetryInterval).To(Equal(90 * time.Second))
}

func TestLoad_DatabaseProviderRetryDefault(t *testing.T) {
	g := NewWithT(t)

	cfg, err := config.Load()
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(cfg.DatabaseProvider.RetryInterval).To(Equal(config.DefaultRetryInterval))
}

// SchemaClaim's retry-interval must stay independently config-driven like
// DatabaseService, so the three-layer precedence is exercised explicitly.

func TestLoad_SchemaClaimRetryDefault(t *testing.T) {
	g := NewWithT(t)

	cfg, err := config.Load()
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(cfg.SchemaClaim.RetryInterval).To(Equal(config.DefaultRetryInterval))
}

func TestLoad_SchemaClaimRetry_ConfigMapOverride(t *testing.T) {
	g := NewWithT(t)

	cfg, err := config.Load(config.WithFS(fstest.MapFS{
		config.KeySchemaClaimRetryInterval: {Data: []byte("7m")},
	}))
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(cfg.SchemaClaim.RetryInterval).To(Equal(7 * time.Minute))
}

func TestLoad_SchemaClaimRetry_EnvOverridesConfigMap(t *testing.T) {
	g := NewWithT(t)

	t.Setenv("ODH_MODULE_OPERATOR_SCHEMACLAIM_RETRY_INTERVAL", "90s")

	cfg, err := config.Load(config.WithFS(fstest.MapFS{
		config.KeySchemaClaimRetryInterval: {Data: []byte("3m")},
	}))
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(cfg.SchemaClaim.RetryInterval).To(Equal(90 * time.Second))
}

func TestLoad_UsesExplicitConfigPathFromStructOption(t *testing.T) {
	g := NewWithT(t)

	dir := t.TempDir()
	path := filepath.Join(dir, config.KeyPlatformType)
	g.Expect(os.WriteFile(path, []byte(config.PlatformTypeManagedRhoai), 0o600)).To(Succeed())

	cfg, err := config.Load(config.LoadOptions{ConfigPath: dir})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.PlatformType).To(Equal(config.PlatformTypeManagedRhoai))
}

func TestLoad_UsesEnvConfigPathByDefault(t *testing.T) {
	g := NewWithT(t)

	dir := t.TempDir()
	path := filepath.Join(dir, config.KeyPlatformVersion)
	g.Expect(os.WriteFile(path, []byte("4.2.0"), 0o600)).To(Succeed())
	t.Setenv(config.ConfigPathEnvVar, dir)

	cfg, err := config.Load()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.PlatformVersion.String()).To(Equal("4.2.0"))
}

// ConfigPathEnvVar is documented as accepting either a ConfigMap directory
// or a single config file -- the latter wasn't exercised anywhere else
// above, and wrapping a file path in os.DirFS used to fail at fs.ReadDir.
func TestLoad_UsesEnvConfigPathPointingAtASingleFile(t *testing.T) {
	g := NewWithT(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	g.Expect(os.WriteFile(path, []byte("platformType: "+config.PlatformTypeManagedRhoai+"\n"), 0o600)).To(Succeed())
	t.Setenv(config.ConfigPathEnvVar, path)

	cfg, err := config.Load()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.PlatformType).To(Equal(config.PlatformTypeManagedRhoai))
}

// PlatformType/PlatformVersion are camelCase config keys with no "-"/"."
// separator, so Viper's automatic env-key derivation (uppercase + replacer)
// produces ODH_MODULE_OPERATOR_PLATFORMTYPE, not the documented
// ODH_MODULE_OPERATOR_PLATFORM_TYPE. Exercised here with no competing flag,
// unlike TestLoad_FlagsOverrideEnvAndConfigMap in flags_test.go, whose flags
// mask this env var and would pass even if the binding were wrong.
func TestLoad_PlatformTypeEnvOverride(t *testing.T) {
	g := NewWithT(t)

	t.Setenv("ODH_MODULE_OPERATOR_PLATFORM_TYPE", config.PlatformTypeSelfManagedRhoai)

	cfg, err := config.Load()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.PlatformType).To(Equal(config.PlatformTypeSelfManagedRhoai))
}

// An unreadable ConfigMap key (permission error, broken symlink, etc.) used
// to be silently skipped by loadFromFS, so the operator could start on a
// different effective configuration than what was actually mounted, with no
// indication anything went wrong.
func TestLoad_UnreadableConfigFileReturnsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions aren't enforced when running as root")
	}

	g := NewWithT(t)

	dir := t.TempDir()
	path := filepath.Join(dir, config.KeyPlatformType)
	g.Expect(os.WriteFile(path, []byte(config.PlatformTypeManagedRhoai), 0o000)).To(Succeed())
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) }) // t.TempDir() cleanup needs to be able to remove it

	_, err := config.Load(config.LoadOptions{ConfigPath: dir})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(config.KeyPlatformType))
}

func TestLoad_PlatformVersionEnvOverride(t *testing.T) {
	g := NewWithT(t)

	t.Setenv("ODH_MODULE_OPERATOR_PLATFORM_VERSION", "5.6.0")

	cfg, err := config.Load()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.PlatformVersion.String()).To(Equal("5.6.0"))
}
