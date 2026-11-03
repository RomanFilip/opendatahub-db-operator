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

package postgres_test

import (
	"context"
	"fmt"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/gomega"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

// startPostgres starts an ephemeral Postgres container and returns the Config.
// The container uses the stock postgres:16 image.
func startPostgres(t *testing.T) postgres.Config {
	t.Helper()
	g := NewWithT(t)

	ctr, err := tcpostgres.Run(t.Context(), "postgres:16",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpassword"),
		tcpostgres.BasicWaitStrategies(), // waits for log "ready" twice + port up
	)
	g.Expect(err).NotTo(HaveOccurred())

	t.Cleanup(func() {
		_ = ctr.Terminate(context.Background())
	})

	connStr, err := ctr.ConnectionString(t.Context(), "sslmode=disable")
	g.Expect(err).NotTo(HaveOccurred())

	cfg, err := postgres.ConfigFromDSN(connStr)
	g.Expect(err).NotTo(HaveOccurred())

	return cfg
}

// TestParseSecret_MissingKeys verifies that each required key absence
// produces a clear error before any connection is attempted.
func TestParseSecret_MissingKeys(t *testing.T) {
	complete := map[string][]byte{
		postgres.SecretKeyHost:     []byte("localhost"),
		postgres.SecretKeyPort:     []byte("5432"),
		postgres.SecretKeyUser:     []byte("admin"),
		postgres.SecretKeyPassword: []byte("secret"),
		postgres.SecretKeyDatabase: []byte("postgres"),
	}

	for _, omit := range []string{
		postgres.SecretKeyHost,
		postgres.SecretKeyUser,
		postgres.SecretKeyPassword,
		postgres.SecretKeyDatabase,
	} {
		t.Run("missing-"+omit, func(t *testing.T) {
			g := NewWithT(t)
			data := make(map[string][]byte)
			for k, v := range complete {
				if k != omit {
					data[k] = v
				}
			}
			_, err := postgres.ParseSecret(data)
			g.Expect(err).To(MatchError(ContainSubstring(omit)))
		})
	}
}

func TestParseSecret_InvalidPort(t *testing.T) {
	g := NewWithT(t)
	_, err := postgres.ParseSecret(map[string][]byte{
		postgres.SecretKeyHost:     []byte("localhost"),
		postgres.SecretKeyPort:     []byte("notanumber"),
		postgres.SecretKeyUser:     []byte("u"),
		postgres.SecretKeyPassword: []byte("p"),
		postgres.SecretKeyDatabase: []byte("d"),
	})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err).To(MatchError(ContainSubstring(postgres.SecretKeyPort)))
}

func TestParseSecret_DefaultPort(t *testing.T) {
	g := NewWithT(t)
	cfg, err := postgres.ParseSecret(map[string][]byte{
		postgres.SecretKeyHost:     []byte("localhost"),
		postgres.SecretKeyUser:     []byte("u"),
		postgres.SecretKeyPassword: []byte("p"),
		postgres.SecretKeyDatabase: []byte("d"),
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.Port).To(Equal(postgres.DefaultPort))
}

func TestParseAdminSecret_AllowsMissingDatabase(t *testing.T) {
	g := NewWithT(t)
	cfg, err := postgres.ParseAdminSecret(map[string][]byte{
		postgres.SecretKeyHost:     []byte("localhost"),
		postgres.SecretKeyUser:     []byte("u"),
		postgres.SecretKeyPassword: []byte("p"),
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.DBName).To(BeEmpty())
}

// TestParseSecret_DecodesPasswordIntoWrapper verifies the Secret's password
// bytes land in the typed wrapper and stay readable through Reveal.
func TestParseSecret_DecodesPasswordIntoWrapper(t *testing.T) {
	g := NewWithT(t)

	cfg, err := postgres.ParseSecret(map[string][]byte{
		postgres.SecretKeyHost:     []byte("localhost"),
		postgres.SecretKeyUser:     []byte("u"),
		postgres.SecretKeyPassword: []byte("decoded-password"),
		postgres.SecretKeyDatabase: []byte("d"),
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.Password.IsZero()).To(BeFalse())
	g.Expect(cfg.Password.Reveal()).To(Equal("decoded-password"))
}

// TestConfig_FormattingRedactsPassword verifies each fmt verb applied to the
// whole config renders the password as [REDACTED] while the rest of the struct
// still prints.
func TestConfig_FormattingRedactsPassword(t *testing.T) {
	const password = "format-sentinel-password"

	cfg := postgres.Config{
		Host:     "db.example.test",
		Port:     5432,
		User:     "postgres",
		Password: secret.FromString(password),
		DBName:   "app-db",
	}

	formats := []struct {
		name  string
		print func(value any) string
	}{
		{name: "verb-v", print: func(value any) string { return fmt.Sprintf("%v", value) }},
		{name: "verb-plus-v", print: func(value any) string { return fmt.Sprintf("%+v", value) }},
		{name: "verb-sharp-v", print: func(value any) string { return fmt.Sprintf("%#v", value) }},
		{name: "Sprint", print: func(value any) string { return fmt.Sprint(value) }},
	}

	for _, format := range formats {
		t.Run(format.name, func(t *testing.T) {
			g := NewWithT(t)

			out := format.print(cfg)

			g.Expect(out).To(ContainSubstring(cfg.Host))
			g.Expect(out).To(ContainSubstring("[REDACTED]"))
			g.Expect(out).NotTo(ContainSubstring(password))
		})
	}
}

// TestConfigDSN_OmitsPassword verifies the DSN carries no credential even
// though the wrapper still holds the real value, and that the user component is
// still escaped.
func TestConfigDSN_OmitsPassword(t *testing.T) {
	g := NewWithT(t)

	cfg := postgres.Config{
		Host:     "db.example.test",
		Port:     5432,
		User:     "user name",
		Password: secret.FromString("pa ss:wo/rd?&=#"),
		DBName:   "app-db",
		SSLMode:  "disable",
	}

	dsn := cfg.DSN()

	g.Expect(cfg.Password.Reveal()).To(Equal("pa ss:wo/rd?&=#"))
	g.Expect(dsn).NotTo(ContainSubstring(cfg.Password.Reveal()))

	dsnURL, err := url.Parse(dsn)
	g.Expect(err).NotTo(HaveOccurred())
	_, hasPassword := dsnURL.User.Password()
	g.Expect(hasPassword).To(BeFalse())

	parsed, err := pgxpool.ParseConfig(dsn)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(parsed.ConnConfig.Host).To(Equal(cfg.Host))
	g.Expect(parsed.ConnConfig.Port).To(Equal(uint16(cfg.Port)))
	g.Expect(parsed.ConnConfig.User).To(Equal(cfg.User))
	g.Expect(parsed.ConnConfig.Database).To(Equal(cfg.DBName))
}

// TestParseSecret_ValidationErrorOmitsPassword verifies a validation failure
// names the offending key and never echoes a Secret value.
func TestParseSecret_ValidationErrorOmitsPassword(t *testing.T) {
	const password = "validation-sentinel-password"

	cases := []struct {
		name    string
		data    map[string][]byte
		wantKey string
	}{
		{
			name: "missing-user",
			data: map[string][]byte{
				postgres.SecretKeyHost:     []byte("localhost"),
				postgres.SecretKeyPassword: []byte(password),
				postgres.SecretKeyDatabase: []byte("d"),
			},
			wantKey: postgres.SecretKeyUser,
		},
		{
			name: "invalid-port",
			data: map[string][]byte{
				postgres.SecretKeyHost:     []byte("localhost"),
				postgres.SecretKeyPort:     []byte("notanumber"),
				postgres.SecretKeyUser:     []byte("u"),
				postgres.SecretKeyPassword: []byte(password),
				postgres.SecretKeyDatabase: []byte("d"),
			},
			wantKey: postgres.SecretKeyPort,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			_, err := postgres.ParseSecret(tc.data)

			g.Expect(err).To(MatchError(ContainSubstring(tc.wantKey)))
			g.Expect(err).NotTo(MatchError(ContainSubstring(password)))
		})
	}
}

// TestClientPing_Success tests a real connection against a testcontainers Postgres.
func TestClientPing_Success(t *testing.T) {
	g := NewWithT(t)
	cfg := startPostgres(t)

	cli, err := postgres.NewClient(t.Context(), cfg)
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(cli.Close)

	err = cli.Ping(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
}

// TestClientPing_WrongPassword verifies that a failed connection returns an error
// that does NOT contain the password literal.
func TestClientPing_WrongPassword(t *testing.T) {
	g := NewWithT(t)
	cfg := startPostgres(t)

	bad := cfg
	bad.Password = secret.FromString("totally-wrong-password-sentinel")

	cli, err := postgres.NewClient(t.Context(), bad)
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(cli.Close)

	err = cli.Ping(t.Context())
	g.Expect(err).To(HaveOccurred())
	g.Expect(err).NotTo(MatchError(ContainSubstring(bad.Password.Reveal())),
		"error message must not contain the password literal")
}
