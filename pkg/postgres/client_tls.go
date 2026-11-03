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
	"crypto/x509"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/tracelog"
)

// poolConfigFor builds the pgx pool configuration for cfg. It is the pgx
// boundary for credential material: pgx keeps the password as a plain string in
// ConnConfig, so this assignment is the only reveal that hands the credential
// to pgx and
// every caller passes the wrapped Config instead of revealing it first.
func poolConfigFor(cfg Config) (*pgxpool.Config, error) {
	poolConfig, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("parsing DSN: %w", err)
	}
	poolConfig.ConnConfig.Password = cfg.Password.Reveal()
	// tracelog emits statement text at Info and pool acquire/release at Debug,
	// so the operator's redaction sits in front of every level it enables.
	poolConfig.ConnConfig.Tracer = &tracelog.TraceLog{
		Logger:   newRedactingTraceLogger(cfg.Password, cfg.traceLogger()),
		LogLevel: cfg.traceLogLevel(),
	}
	if err := applyRuntimeTLSConfig(
		poolConfig.ConnConfig,
		cfg.SSLMode,
		cfg.SSLRootCert,
		cfg.TLSConfigMutator,
	); err != nil {
		return nil, err
	}
	return poolConfig, nil
}

func applyRuntimeTLSConfig(
	connConfig *pgx.ConnConfig,
	sslMode string,
	sslRootCert string,
	mutator func(*tls.Config),
) error {
	if connConfig == nil {
		return nil
	}

	tlsConfig, err := runtimeTLSConfig(connConfig.TLSConfig, connConfig.Host, sslMode, sslRootCert, mutator)
	if err != nil {
		return err
	}
	connConfig.TLSConfig = tlsConfig

	for _, fallback := range connConfig.Fallbacks {
		tlsConfig, err := runtimeTLSConfig(fallback.TLSConfig, fallback.Host, sslMode, sslRootCert, mutator)
		if err != nil {
			return err
		}
		fallback.TLSConfig = tlsConfig
	}

	return nil
}

func runtimeTLSConfig(
	base *tls.Config,
	host string,
	sslMode string,
	sslRootCert string,
	mutator func(*tls.Config),
) (*tls.Config, error) {
	if base == nil {
		return base, nil
	}
	if sslRootCert == "" {
		tlsConfig := base.Clone()
		if mutator != nil {
			mutator(tlsConfig)
		}
		return tlsConfig, nil
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM([]byte(sslRootCert)) {
		return nil, errors.New("unable to add CA to cert pool")
	}

	tlsConfig := base.Clone()
	tlsConfig.RootCAs = caCertPool
	tlsConfig.ClientCAs = caCertPool

	mode := sslMode
	if mode == SSLModeRequire {
		mode = SSLModeVerifyCA
	}

	switch mode {
	case SSLModeVerifyCA:
		// nosemgrep: go-tls-bypass -- verify-ca skips hostname check; verifyCertificateChain below does real CA validation.
		tlsConfig.InsecureSkipVerify = true
		tlsConfig.VerifyPeerCertificate = verifyCertificateChain(tlsConfig)
	case SSLModeVerifyFull:
		tlsConfig.InsecureSkipVerify = false
		tlsConfig.VerifyPeerCertificate = nil
		if tlsConfig.ServerName == "" {
			tlsConfig.ServerName = host
		}
	default:
		tlsConfig.VerifyPeerCertificate = nil
	}
	if mutator != nil {
		mutator(tlsConfig)
	}

	return tlsConfig, nil
}

func verifyCertificateChain(tlsConfig *tls.Config) func([][]byte, [][]*x509.Certificate) error {
	return func(certificates [][]byte, _ [][]*x509.Certificate) error {
		if len(certificates) == 0 {
			return errors.New("server sent no certificates")
		}

		certs := make([]*x509.Certificate, len(certificates))
		for i, asn1Data := range certificates {
			cert, err := x509.ParseCertificate(asn1Data)
			if err != nil {
				return fmt.Errorf("failed to parse certificate from server: %w", err)
			}
			certs[i] = cert
		}

		opts := x509.VerifyOptions{
			Roots:         tlsConfig.RootCAs,
			Intermediates: x509.NewCertPool(),
		}
		for _, cert := range certs[1:] {
			opts.Intermediates.AddCert(cert)
		}

		_, err := certs[0].Verify(opts)
		return err
	}
}
