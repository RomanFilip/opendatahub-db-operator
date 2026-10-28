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

package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

// structuredExtensions is the set of file extensions that are parsed as
// structured config (YAML, JSON) rather than simple key-value pairs.
var structuredExtensions = map[string]bool{
	"yaml": true,
	"yml":  true,
	"json": true,
}

func setDefaults(v *viper.Viper) {
	v.SetDefault(KeyOperatorNS, DefaultOperatorNS)
	v.SetDefault(KeyPlatformType, DefaultPlatformType)
	v.SetDefault(KeyPlatformVersion, DefaultPlatformVersion)
	v.SetDefault(KeyDefaultProviderName, DefaultProviderName)

	v.SetDefault(KeyMetricsBindAddr, DefaultMetricsBindAddr)
	v.SetDefault(KeyHealthBindAddr, DefaultHealthBindAddr)
	v.SetDefault(KeyLeaderElectEnabled, DefaultLeaderElectEnabled)
	v.SetDefault(KeyLeaderElectID, DefaultLeaderElectID)
	v.SetDefault(KeyZapLevel, DefaultZapLevel)
	v.SetDefault(KeyZapDevMode, DefaultZapDevMode)
	v.SetDefault(KeyZapEncoder, DefaultZapEncoder)
	v.SetDefault(KeyPprofEnabled, DefaultPprofEnabled)
	v.SetDefault(KeyPprofBindAddr, "")
	v.SetDefault(KeyWebhookEnabled, DefaultWebhookEnabled)
	v.SetDefault(KeyWebhookPort, DefaultWebhookPort)
	v.SetDefault(KeyWebhookCertDir, DefaultWebhookCertDir)

	v.SetDefault(KeyDatabaseServiceRetryInterval, DefaultRetryInterval)
	v.SetDefault(KeyDatabaseProviderRetryInterval, DefaultRetryInterval)
	v.SetDefault(KeyDatabaseClaimRetryInterval, DefaultRetryInterval)
	v.SetDefault(KeySchemaClaimRetryInterval, DefaultRetryInterval)
	v.SetDefault(KeyGracePeriod, DefaultGracePeriod)
	v.SetDefault("postgres-image", DefaultInternalPostgresImage)
}

// BindEnv configures Viper to read environment variables for the given
// keys so decoding through AllSettings() sees env overrides as well.
func BindEnv(
	v *viper.Viper,
	envPrefix string,
	replacer *strings.Replacer,
	keys ...string,
) error {
	if v == nil {
		return fmt.Errorf("viper is nil")
	}

	v.SetEnvPrefix(envPrefix)
	if replacer != nil {
		v.SetEnvKeyReplacer(replacer)
	}
	v.AutomaticEnv()

	for _, key := range keys {
		if err := v.BindEnv(key); err != nil {
			return fmt.Errorf("binding env for key %s: %w", key, err)
		}
	}

	return nil
}

func bindEnv(v *viper.Viper) error {
	// Explicit BindEnv so Unmarshal picks up env vars.
	// AutomaticEnv only works with Get(), not Unmarshal().
	if err := BindEnv(
		v,
		EnvPrefix,
		strings.NewReplacer("-", "_", ".", "_"),
		v.AllKeys()...,
	); err != nil {
		return err
	}

	// KeyPlatformType/KeyPlatformVersion are camelCase with no "-"/"."
	// separator for the replacer above to act on, so Viper's automatic
	// derivation upcases them verbatim (ODH_MODULE_OPERATOR_PLATFORMTYPE)
	// instead of the documented, underscore-separated names. Bind those two
	// explicitly to the names ConfigPathEnvVar/EnvPrefix actually document.
	if err := v.BindEnv(KeyPlatformType, EnvPrefix+"_PLATFORM_TYPE"); err != nil {
		return fmt.Errorf("binding env for key %s: %w", KeyPlatformType, err)
	}
	if err := v.BindEnv(KeyPlatformVersion, EnvPrefix+"_PLATFORM_VERSION"); err != nil {
		return fmt.Errorf("binding env for key %s: %w", KeyPlatformVersion, err)
	}

	return nil
}

// Decode decodes Viper settings into target using mapstructure.
func Decode(
	v *viper.Viper,
	target any,
	hooks ...mapstructure.DecodeHookFunc,
) error {
	if v == nil {
		return fmt.Errorf("viper is nil")
	}
	if target == nil {
		return fmt.Errorf("decode target is nil")
	}

	allHooks := make([]mapstructure.DecodeHookFunc, 0, 1+len(hooks))
	allHooks = append(allHooks, mapstructure.StringToTimeDurationHookFunc())
	allHooks = append(allHooks, hooks...)

	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           target,
		WeaklyTypedInput: true,
		TagName:          "mapstructure",
		DecodeHook:       mapstructure.ComposeDecodeHookFunc(allHooks...),
	})
	if err != nil {
		return fmt.Errorf("building config decoder: %w", err)
	}
	if err := decoder.Decode(v.AllSettings()); err != nil {
		return fmt.Errorf("decoding config settings: %w", err)
	}

	return nil
}

// loadFromFS reads all files from the given fs.FS into a temporary viper
// instance, then merges the result into v. Structured files (YAML/JSON)
// are parsed normally. Plain files use the filename as a dot-separated
// key path (e.g. "controller.zap.level" expands to a nested map).
// The single MergeConfigMap at the end writes to viper's config layer,
// so environment variables still take precedence.
func loadFromFS(v *viper.Viper, fsys fs.FS) error {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return fmt.Errorf("reading config directory: %w", err)
	}

	tmp := viper.New()

	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		data, err := fs.ReadFile(fsys, entry.Name())
		if err != nil {
			return fmt.Errorf("reading config file %s: %w", entry.Name(), err)
		}

		ext := strings.TrimPrefix(filepath.Ext(entry.Name()), ".")

		if structuredExtensions[ext] {
			if err := mergeStructuredFile(tmp, entry.Name(), ext, data); err != nil {
				return err
			}
		} else {
			tmp.Set(entry.Name(), strings.TrimSpace(string(data)))
		}
	}

	if err := v.MergeConfigMap(tmp.AllSettings()); err != nil {
		return fmt.Errorf("merging config from filesystem: %w", err)
	}

	return nil
}

// loadFromFile reads a single structured (YAML/JSON) config file and merges
// its keys into v -- the ConfigPathEnvVar-points-at-a-file case, as opposed
// to loadFromFS's ConfigMap-directory case.
func loadFromFile(v *viper.Viper, path string) error {
	ext := strings.TrimPrefix(filepath.Ext(path), ".")
	if !structuredExtensions[ext] {
		return fmt.Errorf("unsupported config file extension %q for %s", ext, path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config file %s: %w", path, err)
	}

	return mergeStructuredFile(v, path, ext, data)
}

// mergeStructuredFile parses a YAML/JSON file and merges its keys into viper.
func mergeStructuredFile(v *viper.Viper, name string, ext string, data []byte) error {
	fv := viper.New()
	fv.SetConfigType(ext)

	if err := fv.ReadConfig(strings.NewReader(string(data))); err != nil {
		return fmt.Errorf("parsing config file %s: %w", name, err)
	}

	if err := v.MergeConfigMap(fv.AllSettings()); err != nil {
		return fmt.Errorf("merging config from %s: %w", name, err)
	}

	return nil
}
