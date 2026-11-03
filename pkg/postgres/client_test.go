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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/gomega"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

func startPingPostgres(t *testing.T) Config {
	t.Helper()

	g := NewWithT(t)
	ctr, err := tcpostgres.Run(t.Context(), "postgres:16",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpassword"),
		tcpostgres.BasicWaitStrategies(),
	)
	g.Expect(err).NotTo(HaveOccurred())

	t.Cleanup(func() {
		_ = ctr.Terminate(context.Background())
	})

	connStr, err := ctr.ConnectionString(t.Context(), "sslmode=disable")
	g.Expect(err).NotTo(HaveOccurred())

	cfg, err := ConfigFromDSN(connStr)
	g.Expect(err).NotTo(HaveOccurred())
	return cfg
}

func TestClient_Ping_Success(t *testing.T) {
	g := NewWithT(t)
	cfg := startPingPostgres(t)

	cli, err := NewClient(t.Context(), cfg)
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(cli.Close)

	err = cli.Ping(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
}

func TestClient_Ping_WrongPassword(t *testing.T) {
	g := NewWithT(t)
	cfg := startPingPostgres(t)

	bad := cfg
	bad.Password = secret.FromString("totally-wrong-password-sentinel")

	cli, err := NewClient(t.Context(), bad)
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(cli.Close)

	err = cli.Ping(t.Context())
	g.Expect(err).To(HaveOccurred())
	g.Expect(err).NotTo(MatchError(ContainSubstring(bad.Password.Reveal())))

	// The server's rejection stays classifiable after projection.
	var safe *SafeError
	g.Expect(errors.As(err, &safe)).To(BeTrue())
	g.Expect(safe.RetryClass()).To(Equal(RetryNone))
	g.Expect(IsPgError(err, pgerrcode.InvalidPassword)).To(BeTrue(), "SQLSTATE survives projection")
	g.Expect(err.Error()).To(ContainSubstring("ping failed"))
}

// TestClient_ExecProjectsServerError keeps a real statement rejection
// recognizable by SQLSTATE, and exact: undefined_table is not undefined_object.
func TestClient_ExecProjectsServerError(t *testing.T) {
	g := NewWithT(t)
	cfg := startPingPostgres(t)

	cli, err := NewClient(t.Context(), cfg)
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(cli.Close)

	_, err = cli.Exec(t.Context(), "SELECT * FROM client_api_missing_table")
	g.Expect(err).To(HaveOccurred())
	g.Expect(IsPgError(err, pgerrcode.UndefinedTable)).To(BeTrue())
	g.Expect(IsUndefinedObject(err)).To(BeFalse(), "classification does not blur into a neighbouring code")
	g.Expect(err.Error()).To(ContainSubstring("client_api_missing_table"))
	g.Expect(err.Error()).NotTo(ContainSubstring(cfg.Password.Reveal()))

	// QueryRow defers the server error until Scan; it must be projected too.
	row, err := cli.QueryRow(t.Context(), "SELECT * FROM client_api_missing_table")
	g.Expect(err).NotTo(HaveOccurred())

	var value int
	g.Expect(IsPgError(row.Scan(&value), pgerrcode.UndefinedTable)).To(BeTrue())
}

// TestClient_ExecPasswordStatementRendersNoSecret runs the statement shape ddl.go
// builds: the password travels as a SQL literal, the server rejects the
// statement, and the projected error keeps neither the literal nor a raw cause.
func TestClient_ExecPasswordStatementRendersNoSecret(t *testing.T) {
	g := NewWithT(t)
	cfg := startPingPostgres(t)

	cli, err := NewClient(t.Context(), cfg)
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(cli.Close)

	password := secret.FromString(quotingPasswordSentinel)
	literal := QuoteLiteral(password.Reveal())
	// The server quotes the literal it was given back into the message, the way
	// a statement-derived diagnostic can.
	statement := fmt.Sprintf(
		`DO $$ BEGIN RAISE EXCEPTION 'role creation failed for %%', %s; END $$`,
		literal,
	)

	_, err = cli.Exec(withQuerySecrets(t.Context(), password), statement)
	g.Expect(err).To(HaveOccurred())
	g.Expect(IsPgError(err, pgerrcode.RaiseException)).To(BeTrue())
	g.Expect(err.Error()).To(ContainSubstring("role creation failed"))
	g.Expect(err.Error()).To(ContainSubstring("[REDACTED]"))
	g.Expect(err.Error()).NotTo(ContainSubstring(password.Reveal()))
	g.Expect(err.Error()).NotTo(ContainSubstring(literal))

	var pgErr *pgconn.PgError
	g.Expect(errors.As(err, &pgErr)).To(BeFalse(), "the driver error, with its InternalQuery, is not reachable")
}

func TestNewClient_UnreachableHostTimesOut(t *testing.T) {
	g := NewWithT(t)

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	cli, err := NewClient(ctx, Config{
		Host:     "203.0.113.1",
		Port:     5432,
		User:     "user",
		Password: secret.FromString("secret"),
		DBName:   "db",
	})
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(cli.Close)

	err = cli.Ping(ctx)

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(SatisfyAny(
		ContainSubstring("timeout"),
		ContainSubstring("deadline exceeded"),
	))
}

// closedPort returns a loopback port nothing is listening on, so a dial fails
// with ECONNREFUSED the way it does against a restarting server.
func closedPort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}

	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("closing the reserved port: %v", err)
	}

	return port
}

// TestClientPing_ConnectionRefusedProjectsSafeError covers the classification a
// reconciler acts on: a refused connection stays recognizable as retryable after
// the driver error is projected, and the message names no credential.
func TestClientPing_ConnectionRefusedProjectsSafeError(t *testing.T) {
	g := NewWithT(t)

	cfg := Config{
		Host:     "127.0.0.1",
		Port:     closedPort(t),
		User:     "admin",
		Password: secret.FromString(quotingPasswordSentinel),
		DBName:   "postgres",
		SSLMode:  SSLModeDisable,
	}

	cli, err := NewClient(t.Context(), cfg)
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(cli.Close)

	err = cli.Ping(t.Context())
	g.Expect(err).To(HaveOccurred())
	g.Expect(RetryClassOf(err)).To(Equal(RetryQuick))
	g.Expect(err.Error()).To(ContainSubstring("connection refused"))
	g.Expect(err.Error()).NotTo(ContainSubstring(quotingPasswordSentinel))

	var safe *SafeError
	g.Expect(errors.As(err, &safe)).To(BeTrue())
	g.Expect(safe.SQLState()).To(BeEmpty(), "the failure never reached the server")
	g.Expect(safe.RetryClass()).To(Equal(RetryQuick))

	// Exec and QueryRow fail the same way and must classify alike.
	_, err = cli.Exec(t.Context(), "SELECT 1")
	g.Expect(err).To(HaveOccurred())
	g.Expect(RetryClassOf(err)).To(Equal(RetryQuick))
	g.Expect(err.Error()).To(ContainSubstring("executing statement failed"))

	row, err := cli.QueryRow(t.Context(), "SELECT 1")
	g.Expect(err).NotTo(HaveOccurred())

	var one int
	g.Expect(RetryClassOf(row.Scan(&one))).To(Equal(RetryQuick))
}

// TestNewClient_InvalidTLSProfileProjectsSafeError covers the pool-open path: a
// config that cannot become a pool still returns a credential-free SafeError.
func TestNewClient_InvalidTLSProfileProjectsSafeError(t *testing.T) {
	g := NewWithT(t)

	_, err := NewClient(t.Context(), Config{
		Host:        "db.example.test",
		Port:        5432,
		User:        "admin",
		Password:    secret.FromString(quotingPasswordSentinel),
		DBName:      "postgres",
		SSLMode:     SSLModeVerifyFull,
		SSLRootCert: "not a certificate",
	})

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("building pool config failed"))
	g.Expect(err.Error()).NotTo(ContainSubstring(quotingPasswordSentinel))
	g.Expect(RetryClassOf(err)).To(Equal(RetryNone))

	var safe *SafeError
	g.Expect(errors.As(err, &safe)).To(BeTrue())
}

func TestNewClient_ExecQueryAndQueryRow(t *testing.T) {
	g := NewWithT(t)
	cfg := startPingPostgres(t)

	cli, err := NewClient(t.Context(), cfg)
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(cli.Close)

	_, err = cli.Exec(t.Context(), "CREATE TABLE IF NOT EXISTS client_api_test (id int)")
	g.Expect(err).NotTo(HaveOccurred())

	_, err = cli.Exec(t.Context(), "TRUNCATE client_api_test")
	g.Expect(err).NotTo(HaveOccurred())

	tag, err := cli.Exec(t.Context(), "INSERT INTO client_api_test (id) VALUES ($1)", 7)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(tag).To(Equal(pgconn.NewCommandTag("INSERT 0 1")))

	var count int
	row, err := cli.QueryRow(t.Context(), "SELECT count(*) FROM client_api_test")
	g.Expect(err).NotTo(HaveOccurred())
	err = row.Scan(&count)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(count).To(Equal(1))

	rows, err := cli.Query(t.Context(), "SELECT id FROM client_api_test")
	g.Expect(err).NotTo(HaveOccurred())
	defer rows.Close()

	g.Expect(rows.Next()).To(BeTrue())

	var id int
	err = rows.Scan(&id)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(id).To(Equal(7))
	g.Expect(rows.Next()).To(BeFalse())
	g.Expect(rows.Err()).NotTo(HaveOccurred())
}

func TestClient_CloseMakesFurtherOperationsFail(t *testing.T) {
	g := NewWithT(t)
	cfg := startPingPostgres(t)

	cli, err := NewClient(t.Context(), cfg)
	g.Expect(err).NotTo(HaveOccurred())
	cli.Close()
	cli.Close()

	_, err = cli.Exec(t.Context(), "SELECT 1")
	g.Expect(err).To(MatchError(ContainSubstring("not open")))

	row, err := cli.QueryRow(t.Context(), "SELECT 1")
	g.Expect(row).To(BeNil())
	g.Expect(err).To(MatchError(ContainSubstring("not open")))
}

func TestPoolConfigFor_ConfiguresVerifyFullFromCAData(t *testing.T) {
	g := NewWithT(t)

	poolConfig, err := poolConfigFor(Config{
		Host:        "db.example.test",
		Port:        5432,
		User:        "user",
		Password:    secret.FromString("secret"),
		DBName:      "db",
		SSLMode:     SSLModeVerifyFull,
		SSLRootCert: string(testRootCA(t)),
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(poolConfig.ConnConfig.TLSConfig).NotTo(BeNil())
	g.Expect(poolConfig.ConnConfig.TLSConfig.RootCAs).NotTo(BeNil())
	g.Expect(poolConfig.ConnConfig.TLSConfig.InsecureSkipVerify).To(BeFalse())
	g.Expect(poolConfig.ConnConfig.TLSConfig.VerifyPeerCertificate).To(BeNil())
	g.Expect(poolConfig.ConnConfig.TLSConfig.ServerName).To(Equal("db.example.test"))
	g.Expect(poolConfig.ConnConfig.Password).To(Equal("secret"))
}

func TestPoolConfigFor_ConfiguresVerifyCAFromCAData(t *testing.T) {
	g := NewWithT(t)

	poolConfig, err := poolConfigFor(Config{
		Host:        "db.example.test",
		Port:        5432,
		User:        "user",
		Password:    secret.FromString("secret"),
		DBName:      "db",
		SSLMode:     SSLModeVerifyCA,
		SSLRootCert: string(testRootCA(t)),
	})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(poolConfig.ConnConfig.TLSConfig).NotTo(BeNil())
	g.Expect(poolConfig.ConnConfig.TLSConfig.RootCAs).NotTo(BeNil())
	g.Expect(poolConfig.ConnConfig.TLSConfig.InsecureSkipVerify).To(BeTrue())
	g.Expect(poolConfig.ConnConfig.TLSConfig.VerifyPeerCertificate).NotTo(BeNil())
}

func TestPoolConfigFor_RejectsInvalidCAData(t *testing.T) {
	g := NewWithT(t)

	_, err := poolConfigFor(Config{
		Host:        "db.example.test",
		Port:        5432,
		User:        "user",
		Password:    secret.FromString("secret"),
		DBName:      "db",
		SSLMode:     SSLModeVerifyFull,
		SSLRootCert: "not a pem bundle",
	})

	g.Expect(err).To(MatchError(ContainSubstring("unable to add CA to cert pool")))
}

// TestPoolConfigFor_CarriesPasswordToPgx pins the password boundary for the
// DSN: DSN() omits the credential, so the pool config must receive it from the
// wrapper or connections cannot authenticate. pgx v5 keeps no connection string
// on ConnConfig, so DSN() is the only rendered connection string on this path.
func TestPoolConfigFor_CarriesPasswordToPgx(t *testing.T) {
	g := NewWithT(t)
	const password = "boundary-sentinel-password"

	cfg := Config{
		Host:     "db.example.test",
		Port:     5432,
		User:     "user",
		Password: secret.FromString(password),
		DBName:   "db",
		SSLMode:  SSLModeDisable,
	}

	poolConfig, err := poolConfigFor(cfg)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(poolConfig.ConnConfig.Password).To(Equal(password))
	g.Expect(poolConfig.ConnConfig.Host).To(Equal(cfg.Host))
	g.Expect(poolConfig.ConnConfig.Port).To(Equal(uint16(cfg.Port)))
	g.Expect(poolConfig.ConnConfig.User).To(Equal(cfg.User))
	g.Expect(poolConfig.ConnConfig.Database).To(Equal(cfg.DBName))

	dsn := cfg.DSN()
	g.Expect(dsn).NotTo(ContainSubstring(password))
	g.Expect(dsn).NotTo(ContainSubstring(url.QueryEscape(password)))
	dsnURL, err := url.Parse(dsn)
	g.Expect(err).NotTo(HaveOccurred())
	_, dsnHasPassword := dsnURL.User.Password()
	g.Expect(dsnHasPassword).To(BeFalse())
}

// TestPoolConfigFor_SentinelPasswordStaysOutOfRenderedText uses a password that
// needs URL quoting so a leak through the connection string or a construction
// error would show up as the encoded or raw literal.
func TestPoolConfigFor_SentinelPasswordStaysOutOfRenderedText(t *testing.T) {
	const password = "pa:ss@word/with?reserved&chars#1"

	baseConfig := func() Config {
		return Config{
			Host:     "db.example.test",
			Port:     5432,
			User:     "user",
			Password: secret.FromString(password),
			DBName:   "db",
			SSLMode:  SSLModeVerifyFull,
		}
	}

	t.Run("connection-string", func(t *testing.T) {
		g := NewWithT(t)
		cfg := baseConfig()
		cfg.SSLMode = SSLModeDisable

		poolConfig, err := poolConfigFor(cfg)

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(poolConfig.ConnConfig.Password).To(Equal(password))
		g.Expect(cfg.DSN()).NotTo(ContainSubstring(password))
		dsnURL, err := url.Parse(cfg.DSN())
		g.Expect(err).NotTo(HaveOccurred())
		_, dsnHasPassword := dsnURL.User.Password()
		g.Expect(dsnHasPassword).To(BeFalse())
	})

	t.Run("construction-errors", func(t *testing.T) {
		g := NewWithT(t)
		cfg := baseConfig()
		cfg.SSLRootCert = "not a pem bundle"

		_, poolConfigErr := poolConfigFor(cfg)

		g.Expect(poolConfigErr).To(MatchError(ContainSubstring("unable to add CA to cert pool")))
		g.Expect(poolConfigErr.Error()).NotTo(ContainSubstring(password))

		_, clientErr := NewClient(t.Context(), cfg)

		g.Expect(clientErr).To(MatchError(ContainSubstring("building pool config")))
		g.Expect(clientErr.Error()).NotTo(ContainSubstring(password))
	})
}

func testRootCA(t *testing.T) []byte {
	t.Helper()

	g := NewWithT(t)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	g.Expect(err).NotTo(HaveOccurred())

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "odh-db-operator-test-ca",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	g.Expect(err).NotTo(HaveOccurred())

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
