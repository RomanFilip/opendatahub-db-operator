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

package postgres

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"strconv"

	"github.com/go-logr/logr"
	"github.com/go-viper/mapstructure/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

// Secret key names for connection parameters. Bare (undotted) keys rather than
// PostgreSQL's PGXXX env-var names, so the Secret can be safely projected with
// Kubernetes envFrom (EnvFromSource silently drops keys containing a period).
const (
	SecretKeyHost     = "host"
	SecretKeyPort     = "port"
	SecretKeyUser     = "user"
	SecretKeyPassword = "password"
	SecretKeyDatabase = "dbname"
	SecretKeySchema   = "schema"
	// SecretKeyCA holds PEM-encoded CA data used to verify the server
	// certificate when sslmode is verify-ca or verify-full.
	SecretKeyCA = "ca.crt"
	// SecretKeySSLMode holds the libpq sslmode value (e.g. "disable", "require",
	// "verify-full"). Optional in the Secret; when absent, callers apply their own
	// default ("disable" for Internal, "require" for External).
	SecretKeySSLMode = "sslmode"

	// DefaultPort is the standard PostgreSQL port, used when port is absent.
	DefaultPort          = 5432
	DefaultAdminDatabase = "postgres"

	// SSLMode* are the libpq sslmode values accepted by sslmode.
	SSLModeDisable    = "disable"
	SSLModeRequire    = "require"
	SSLModeVerifyCA   = "verify-ca"
	SSLModeVerifyFull = "verify-full"

	// AnnotationConnectionTypeProtocol is the ODH Connection API annotation
	// that marks a Secret as a postgres connection artifact.
	AnnotationConnectionTypeProtocol = "opendatahub.io/connection-type-protocol"
	// ConnectionTypeProtocolPostgres is the annotation value for PostgreSQL.
	ConnectionTypeProtocolPostgres = "postgres"
)

// Config holds the connection parameters parsed from or written to a Kubernetes
// Secret. The bare-key convention keeps the Secret envFrom-safe. The
// mapstructure tags match the SecretKey*
// constants above. Schema and SSLMode are optional.
type Config struct {
	Host     string              `mapstructure:"host"`
	Port     int                 `mapstructure:"port"`
	User     string              `mapstructure:"user"`
	Password secret.SecretString `mapstructure:"password"`
	DBName   string              `mapstructure:"dbname"`
	Schema   string              `mapstructure:"schema"`
	// SSLRootCert stores PEM-encoded CA content from SecretKeyCA. Runtime
	// helpers build the pgx TLS configuration directly from this content.
	SSLRootCert      string            `mapstructure:"ca.crt"`
	SSLMode          string            `mapstructure:"sslmode"`
	TLSConfigMutator func(*tls.Config) `mapstructure:"-"`
	// Logger receives this connection's pgx trace events. It never comes from a
	// Secret: the controller layer copies it from the operator logger, and a
	// zero Logger discards trace output.
	Logger logr.Logger `mapstructure:"-"`
	// TraceQueries promotes pgx trace output from database errors to every
	// statement. The controller layer sets it from the operator log level, so a
	// production operator does not log each statement it issues.
	TraceQueries bool `mapstructure:"-"`
}

func (c Config) TLSEnabled() bool {
	return c.SSLMode != "" && c.SSLMode != SSLModeDisable
}

// TLSReady reports whether the config includes the trust material required by
// the selected sslmode.
func (c Config) TLSReady() bool {
	switch c.SSLMode {
	case SSLModeVerifyCA, SSLModeVerifyFull:
		return c.SSLRootCert != ""
	case "", SSLModeDisable:
		return false
	default:
		return true
	}
}

// secretField reports whether a required Secret key is absent without carrying
// its value, so a validation error can name the key but never echo the value.
type secretField struct {
	key     string
	missing bool
}

func (c Config) validate(requireDatabase bool) error {
	for _, field := range [...]secretField{
		{SecretKeyHost, c.Host == ""},
		{SecretKeyUser, c.User == ""},
		{SecretKeyPassword, c.Password.IsZero()},
	} {
		if field.missing {
			return fmt.Errorf("missing or empty key %q in Secret", field.key)
		}
	}
	if requireDatabase && c.DBName == "" {
		return fmt.Errorf("missing or empty key %q in Secret", SecretKeyDatabase)
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("invalid %s: must be a port number 1-65535, got %d", SecretKeyPort, c.Port)
	}
	return nil
}

// Validate checks that all required Config fields are set and the port is valid.
func (c Config) Validate() error {
	return c.validate(true)
}

// ValidateWithoutDatabase checks that all required Config fields except the
// database name are set. Use this only for provider/admin secret loading paths
// that resolve the effective database from higher-level config.
func (c Config) ValidateWithoutDatabase() error {
	return c.validate(false)
}

var (
	bytesType        = reflect.TypeFor[[]byte]()
	secretStringType = reflect.TypeFor[secret.SecretString]()
)

// decodeSecretValue builds the typed password wrapper straight from Secret
// bytes, and otherwise promotes []byte -> string so that WeaklyTypedInput can
// handle string -> int for PGPORT.
func decodeSecretValue(from, to reflect.Type, v any) (any, error) {
	if to == secretStringType {
		switch value := v.(type) {
		case []byte:
			return secret.FromBytes(value), nil
		case string:
			return secret.FromString(value), nil
		default:
			return v, nil
		}
	}
	if from == bytesType {
		return string(v.([]byte)), nil
	}
	return v, nil
}

// ParseSecret decodes a Secret's data map ([]byte values) into a Config using
// mapstructure. PGPORT defaults to DefaultPort when absent.
func parseSecret(data map[string][]byte, requireDatabase bool) (Config, error) {
	cfg := Config{Port: DefaultPort}

	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           &cfg,
		WeaklyTypedInput: true, // converts string "5432" -> int
		TagName:          "mapstructure",
		DecodeHook:       decodeSecretValue,
	})
	if err != nil {
		return cfg, fmt.Errorf("building decoder: %w", err)
	}
	if err := dec.Decode(data); err != nil {
		return cfg, fmt.Errorf("decoding Secret fields: %w", err)
	}

	if requireDatabase {
		return cfg, cfg.Validate()
	}
	return cfg, cfg.ValidateWithoutDatabase()
}

func ParseSecret(data map[string][]byte) (Config, error) {
	return parseSecret(data, true)
}

// ParseAdminSecret decodes a provider/admin Secret that may intentionally omit
// dbname. Use this only when another config source will provide the
// effective database before the returned config is used.
func ParseAdminSecret(data map[string][]byte) (Config, error) {
	return parseSecret(data, false)
}

// ConfigFromDSN parses a libpq or postgres:// DSN into a Config using
// pgxpool's own parser -- no manual URL decomposition needed. Useful in tests
// where the DSN comes from a testcontainers ConnectionString() call.
func ConfigFromDSN(dsn string) (Config, error) {
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return Config{}, fmt.Errorf("parsing DSN: %w", err)
	}
	// pgx does not expose the raw sslmode string after parsing, so extract it
	// directly from the URL query string.
	sslMode := ""
	if u, err := url.Parse(dsn); err == nil {
		sslMode = u.Query().Get("sslmode")
	}
	return Config{
		Host:     pcfg.ConnConfig.Host,
		Port:     int(pcfg.ConnConfig.Port),
		User:     pcfg.ConnConfig.User,
		Password: secret.FromString(pcfg.ConnConfig.Password),
		DBName:   pcfg.ConnConfig.Database,
		SSLMode:  sslMode,
	}, nil
}

// DSN returns the libpq-style connection string for the config. It deliberately
// carries no password; connection setup supplies the credential at the pgx
// boundary instead.
func (c Config) DSN() string {
	q := url.Values{}
	if c.SSLMode != "" {
		q.Set("sslmode", c.SSLMode)
	}
	return (&url.URL{
		Scheme:   "postgres",
		User:     url.User(c.User),
		Host:     net.JoinHostPort(c.Host, strconv.Itoa(c.Port)),
		Path:     c.DBName,
		RawQuery: q.Encode(),
	}).String()
}
