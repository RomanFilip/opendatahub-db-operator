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
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/gomega"

	"github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

// quotingPasswordSentinel needs QuoteLiteral escaping, so a projection that
// replaced only the raw form would still leave it in the message.
const quotingPasswordSentinel = `cla'im\pw`

// passwordBearingServerError models the error PostgreSQL returns for the
// statements ddl.go builds: the password literal appears in the fields a raw
// *pgconn.PgError renders and in the fields it does not.
func passwordBearingServerError(password secret.SecretString) *pgconn.PgError {
	statement := "CREATE ROLE app WITH LOGIN PASSWORD " + QuoteLiteral(password.Reveal())

	return &pgconn.PgError{
		Severity:      "ERROR",
		Code:          pgerrcode.DuplicateObject,
		Message:       `role "app" already exists`,
		Detail:        "Key (rolname)=(app) already exists. Statement: " + statement,
		Hint:          "Statement: " + statement,
		Where:         "SQL statement " + statement,
		InternalQuery: statement,
	}
}

func projectedSecrets(password secret.SecretString) boundarySecrets {
	return boundarySecrets{statement: []secret.SecretString{password}}
}

// TestProjectError_RendersNoStatementPassword pins the rendered shape: server
// severity, message, and SQLSTATE only. Detail, Hint, Where, and InternalQuery
// carry the statement, so none of them is rendered.
func TestProjectError_RendersNoStatementPassword(t *testing.T) {
	g := NewWithT(t)
	password := secret.FromString(quotingPasswordSentinel)
	quoted := QuoteLiteral(password.Reveal())

	projected := projectError("creating role", passwordBearingServerError(password), projectedSecrets(password))

	var safe *SafeError
	g.Expect(errors.As(projected, &safe)).To(BeTrue(), "a projected failure must still be identifiable")
	g.Expect(safe.Error()).To(Equal(`creating role failed: ERROR: role "app" already exists (SQLSTATE 42710)`))
	g.Expect(safe.Error()).NotTo(ContainSubstring(quotingPasswordSentinel))
	g.Expect(safe.Error()).NotTo(ContainSubstring(quoted))
	g.Expect(safe.SQLState()).To(Equal(pgerrcode.DuplicateObject))
}

// TestProjectError_RedactsStatementFromDriverText covers the driver failures
// that render text themselves rather than through *pgconn.PgError.
func TestProjectError_RedactsStatementFromDriverText(t *testing.T) {
	g := NewWithT(t)
	password := secret.FromString(quotingPasswordSentinel)

	driverErr := fmt.Errorf(
		"cannot execute statement: CREATE ROLE app WITH LOGIN PASSWORD %s",
		QuoteLiteral(password.Reveal()),
	)

	projected := projectError("executing statement", driverErr, projectedSecrets(password))

	g.Expect(projected.Error()).To(Equal(
		`executing statement failed: cannot execute statement: CREATE ROLE app WITH LOGIN PASSWORD [REDACTED]`,
	))
	g.Expect(projected.Error()).NotTo(ContainSubstring(quotingPasswordSentinel))
	g.Expect(projected.Error()).NotTo(ContainSubstring(QuoteLiteral(quotingPasswordSentinel)))
}

// TestProjectError_WithholdsUnredactableStatementRendering covers a rendering the
// replacement list cannot rewrite but the detector recognizes — a driver that
// renders an argument encoded, here as hex. The text is withheld in favor of the
// classification: a leak is worse than losing diagnostics.
func TestProjectError_WithholdsUnredactableStatementRendering(t *testing.T) {
	g := NewWithT(t)
	password := secret.FromString(quotingPasswordSentinel)
	encoded := hex.EncodeToString([]byte(password.Reveal()))

	projected := projectError("executing statement", errors.New("cannot execute "+encoded), projectedSecrets(password))

	var safe *SafeError
	g.Expect(errors.As(projected, &safe)).To(BeTrue())
	g.Expect(safe.Error()).To(Equal("executing statement failed: error text withheld"))
	g.Expect(safe.SQLState()).To(BeEmpty())

	withState := projectError(
		"executing statement",
		&pgconn.PgError{Code: pgerrcode.UndefinedTable, Message: "table " + encoded + " is missing"},
		projectedSecrets(password),
	)
	g.Expect(withState.Error()).To(Equal("executing statement failed: error text withheld (SQLSTATE 42P01)"))
	g.Expect(IsPgError(withState, pgerrcode.UndefinedTable)).To(BeTrue(), "a withheld message keeps its classification")
}

// TestProjectError_ReplacesPoolCredentialWithoutWithholding covers the asymmetry
// between the two kinds of secret: an administrator password can be an ordinary
// word, so replacing it must not swallow the authentication failure the message
// exists to explain. No pgx rendering of a connection carries it, so the
// replacement is already defense in depth.
func TestProjectError_ReplacesPoolCredentialWithoutWithholding(t *testing.T) {
	g := NewWithT(t)
	password := secret.FromString("postgres")

	projected := projectError(
		"ping",
		&pgconn.PgError{Code: pgerrcode.InvalidPassword, Message: `password authentication failed for user "postgres"`},
		boundarySecrets{pool: password},
	)

	g.Expect(projected.Error()).To(Equal(
		`ping failed: password authentication failed for user "[REDACTED]" (SQLSTATE 28P01)`,
	))
	g.Expect(IsPgError(projected, pgerrcode.InvalidPassword)).To(BeTrue())
}

// TestProjectError_PreservesSQLStateClassification is the contract the
// idempotent DDL paths depend on: a missing database, schema, or role is still
// recognizable after projection.
func TestProjectError_PreservesSQLStateClassification(t *testing.T) {
	testCases := []struct {
		name        string
		code        string
		recognizes  func(error) bool
		alsoMatches func(error) bool
	}{
		{"invalid catalog name", pgerrcode.InvalidCatalogName, IsInvalidCatalogName, IsUndefinedObject},
		{"duplicate schema", pgerrcode.DuplicateSchema, IsDuplicateSchema, IsDuplicateObject},
		{"duplicate object", pgerrcode.DuplicateObject, IsDuplicateObject, IsDuplicateSchema},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			g := NewWithT(t)

			raw := &pgconn.PgError{Code: testCase.code, Message: "object exists"}
			g.Expect(testCase.recognizes(raw)).To(BeTrue(), "the raw driver error classifies")

			projected := ProjectError("executing statement", raw)

			g.Expect(testCase.recognizes(projected)).To(BeTrue(), "the projected error classifies")
			g.Expect(testCase.alsoMatches(projected)).To(BeFalse(), "classification stays exact")
			g.Expect(IsPgError(projected, testCase.code)).To(BeTrue())
			g.Expect(IsPgError(projected, "")).To(BeFalse(), "an empty code is not a wildcard")
		})
	}
}

// TestProjectError_PreservesRetryClass keeps the connection-refused quick retry
// working across the boundary: the *net.OpError chain does not survive, the
// classification does.
func TestProjectError_PreservesRetryClass(t *testing.T) {
	g := NewWithT(t)
	refused := &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: fmt.Errorf("connect: %w", syscall.ECONNREFUSED),
	}

	g.Expect(RetryClassOf(refused)).To(Equal(RetryQuick), "the raw error classifies")

	projected := ProjectError("ping", fmt.Errorf("connecting: %w", refused))

	var safe *SafeError
	g.Expect(errors.As(projected, &safe)).To(BeTrue())
	g.Expect(safe.RetryClass()).To(Equal(RetryQuick))
	g.Expect(RetryClassOf(projected)).To(Equal(RetryQuick))
	g.Expect(safe.Error()).To(ContainSubstring("connection refused"))

	serverErr := ProjectError("executing statement", &pgconn.PgError{Code: pgerrcode.DuplicateObject})
	g.Expect(RetryClassOf(serverErr)).To(Equal(RetryNone))
}

// TestProjectError_PreservesContextSentinels keeps a timeout recognizable
// without exposing the driver text that carried it.
func TestProjectError_PreservesContextSentinels(t *testing.T) {
	g := NewWithT(t)

	projected := ProjectError("ping", fmt.Errorf("connect: %w", context.DeadlineExceeded))

	g.Expect(errors.Is(projected, context.DeadlineExceeded)).To(BeTrue())
	g.Expect(errors.Is(projected, context.Canceled)).To(BeFalse())

	canceled := ProjectError("ping", context.Canceled)
	g.Expect(errors.Is(canceled, context.Canceled)).To(BeTrue())
}

// TestProjectError_KeepsNoRawCause is the reason the projection exists: driver
// text never comes back, not even by unwrapping.
func TestProjectError_KeepsNoRawCause(t *testing.T) {
	g := NewWithT(t)
	password := secret.FromString(quotingPasswordSentinel)
	raw := passwordBearingServerError(password)

	projected := ProjectError("creating role", raw, password)

	var pgErr *pgconn.PgError
	g.Expect(errors.As(projected, &pgErr)).To(BeFalse(), "the driver error must not be reachable")
	g.Expect(errors.Unwrap(projected)).To(Succeed(), "no raw cause is retained")
}

// TestProjectError_IsIdempotent keeps callers that project defensively from
// re-rendering an already safe message.
func TestProjectError_IsIdempotent(t *testing.T) {
	g := NewWithT(t)
	password := secret.FromString(quotingPasswordSentinel)

	once := ProjectError("creating role", passwordBearingServerError(password), password)
	twice := ProjectError("creating role", once, password)

	g.Expect(twice.Error()).To(Equal(once.Error()))
	g.Expect(RetryClassOf(twice)).To(Equal(RetryClassOf(once)))
	g.Expect(IsDuplicateObject(twice)).To(BeTrue())

	wrapped := ProjectError("executing statement", fmt.Errorf("ensuring role: %w", once), password)
	var safe *SafeError
	g.Expect(errors.As(wrapped, &safe)).To(BeTrue())
	g.Expect(wrapped.Error()).To(ContainSubstring(once.Error()), "the caller's context survives")
}

func TestProjectError_NilErrorStaysNil(t *testing.T) {
	g := NewWithT(t)

	g.Expect(ProjectError("ping", nil)).To(Succeed())
}
