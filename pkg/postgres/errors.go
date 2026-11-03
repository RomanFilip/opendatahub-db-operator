package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

// Wording a projected failure renders when it cannot keep the driver's text.
const (
	withheldText  = "error text withheld"
	sqlStateLabel = "SQLSTATE "
)

// sqlStateCarrier is implemented by *pgconn.PgError and *SafeError, so SQLSTATE
// classification reads the same code before and after projection.
type sqlStateCarrier interface {
	SQLState() string
}

// sqlStateOf returns the SQLSTATE err carries, reading it from a projected
// SafeError as well as from the raw driver error.
func sqlStateOf(err error) string {
	var carrier sqlStateCarrier
	if errors.As(err, &carrier) {
		return carrier.SQLState()
	}
	return ""
}

// IsPgError reports whether err carries the given SQLSTATE code. Use
// pgerrcode constants (e.g. pgerrcode.UndefinedObject) as the code.
func IsPgError(err error, code string) bool {
	return code != "" && sqlStateOf(err) == code
}

// IsUndefinedObject reports whether err is SQLSTATE 42704 (undefined_object).
// PostgreSQL returns this for any missing database object — role, schema,
// function, etc. — that a statement references. Callers can use it to
// distinguish "already gone" from real failures and treat it as a no-op.
func IsUndefinedObject(err error) bool {
	return IsPgError(err, pgerrcode.UndefinedObject)
}

// IsInvalidCatalogName reports whether err is SQLSTATE 3D000
// (invalid_catalog_name) — typically a missing database.
func IsInvalidCatalogName(err error) bool {
	return IsPgError(err, pgerrcode.InvalidCatalogName)
}

// IsDuplicateDatabase reports whether err is SQLSTATE 42P04
// (duplicate_database). Concurrent CREATE DATABASE races land here.
func IsDuplicateDatabase(err error) bool {
	return IsPgError(err, pgerrcode.DuplicateDatabase)
}

// IsDuplicateSchema reports whether err is SQLSTATE 42P06
// (duplicate_schema). Concurrent CREATE SCHEMA races land here.
func IsDuplicateSchema(err error) bool {
	return IsPgError(err, pgerrcode.DuplicateSchema)
}

// IsDuplicateObject reports whether err is SQLSTATE 42710 (duplicate_object).
// Concurrent CREATE ROLE races land here.
func IsDuplicateObject(err error) bool {
	return IsPgError(err, pgerrcode.DuplicateObject)
}

// RetryClass is how soon a failed PostgreSQL operation is worth retrying.
type RetryClass int

const (
	// RetryNone means the failure is not known to clear on its own.
	RetryNone RetryClass = iota
	// RetryQuick means the server refused the connection, which a restart or a
	// Service without ready endpoints clears within seconds.
	RetryQuick
)

// RetryClassOf reports how err should be retried. A projected SafeError carries
// the class established where the raw driver error existed; an unprojected
// error is inspected directly, so a caller classifies alike whichever shape it
// receives.
func RetryClassOf(err error) RetryClass {
	var safe *SafeError
	if errors.As(err, &safe) {
		return safe.retry
	}
	return classifyRetry(err)
}

func classifyRetry(err error) RetryClass {
	var opErr *net.OpError
	if errors.As(err, &opErr) && errors.Is(opErr.Err, syscall.ECONNREFUSED) {
		return RetryQuick
	}
	return RetryNone
}

// SafeError is the shape a PostgreSQL failure takes outside the driver
// boundary: a message with credentials removed plus the classification a
// reconciler needs, so no caller unwraps or re-renders driver text.
//
// It keeps no raw cause on purpose. ddl.go interpolates the claim password into
// CREATE ROLE as a SQL literal, and *pgconn.PgError exposes the statement the
// server was executing as InternalQuery, so only SQLSTATE, the retry class, and
// a credential-free message cross this boundary.
type SafeError struct {
	message  string
	sqlState string
	retry    RetryClass
	// cause is a secret-free context sentinel the failure matched, and the only
	// value Unwrap exposes.
	cause error
}

// Error returns the projected message.
func (e *SafeError) Error() string {
	return e.message
}

// SQLState returns the server's SQLSTATE code, or "" when the failure never
// reached the server.
func (e *SafeError) SQLState() string {
	return e.sqlState
}

// RetryClass returns the retry classification established at the boundary.
func (e *SafeError) RetryClass() RetryClass {
	return e.retry
}

// Unwrap exposes the context sentinel the failure matched, so
// errors.Is(err, context.DeadlineExceeded) keeps working. Driver errors are
// never unwrapped: their text can echo statement text.
func (e *SafeError) Unwrap() error {
	return e.cause
}

// ProjectError renders err the way a Client returns it: every rendering of the
// given secrets removed, SQLSTATE and retry class preserved. Client
// implementations outside this package and their test doubles use it so a
// reconciler never has to re-render driver text. Projecting an error whose
// chain already holds a SafeError returns it unchanged.
func ProjectError(operation string, err error, secrets ...secret.SecretString) error {
	return projectError(operation, err, boundarySecrets{statement: secrets})
}

// projectError projects a driver failure at the point where the raw error, the
// pool credential, and the statement-scoped secrets are all in hand.
func projectError(operation string, err error, secrets boundarySecrets) error {
	if err == nil {
		return nil
	}
	var projected *SafeError
	if errors.As(err, &projected) {
		// Callers must not interpolate credentials into wrapper text around an
		// already-projected error; only Client methods call projectError and none do.
		return err
	}
	sqlState := sqlStateOf(err)
	return &SafeError{
		message:  describeFailure(operation, err, sqlState, secrets),
		sqlState: sqlState,
		retry:    classifyRetry(err),
		cause:    contextSentinel(err),
	}
}

// boundarySecrets lists what a driver failure can echo: the pool's connection
// credential, and the secrets a statement interpolated as SQL literals
// (withQuerySecrets, the context values the pool tracer also reads).
type boundarySecrets struct {
	pool      secret.SecretString
	statement []secret.SecretString
}

// scrub removes these credentials from driver text and reports whether a
// statement secret survived in a rendering the redactor does not replace.
//
// The pool credential is replaced but does not fail closed: an
// administrator-chosen password can also be an ordinary word ("postgres"), and
// withholding every message that names the database would hide the
// authentication failures the message exists to explain. Nothing pgx renders
// for a connection carries that credential — ParseConfigError redacts the
// connection string, ConnectError prints only user and database, and DSN()
// never held the password — so replacing it is already defense in depth.
func (s boundarySecrets) scrub(value string) (string, bool) {
	redactor := newTraceRedactor(s.statement...)
	redacted := redactor.redactString(value)
	if redactor.containsSecret(redacted) {
		return "", true
	}

	return newTraceRedactor(s.pool).redactString(redacted), false
}

// describeFailure renders the safe text for a failed operation.
func describeFailure(operation string, err error, sqlState string, secrets boundarySecrets) string {
	text, withheld := failureText(err, secrets)
	if !withheld {
		return fmt.Sprintf("%s failed: %s", operation, text)
	}
	if sqlState != "" {
		return fmt.Sprintf("%s failed: %s (%s%s)", operation, withheldText, sqlStateLabel, sqlState)
	}
	return fmt.Sprintf("%s failed: %s", operation, withheldText)
}

// failureText returns the renderable part of err. A server error contributes
// only Severity, Message, and Code: Detail, Hint, Where, and InternalQuery can
// carry the statement the server was executing, password literal included.
func failureText(err error, secrets boundarySecrets) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return secrets.scrub(pgErrorHeadline(pgErr))
	}
	return secrets.scrub(err.Error())
}

// pgErrorHeadline renders severity, message, and code in the order pgconn's
// Error() states them, omitting empty parts, and names the fields it renders so
// the excluded ones (Detail, Hint, Where, InternalQuery) stay visibly excluded.
func pgErrorHeadline(pgErr *pgconn.PgError) string {
	headline := pgErr.Message
	if pgErr.Severity != "" {
		headline = pgErr.Severity + ": " + headline
	}
	if pgErr.Code != "" {
		headline += " (" + sqlStateLabel + pgErr.Code + ")"
	}
	return headline
}

// contextSentinel returns the secret-free sentinel err matched, if any.
func contextSentinel(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		return context.Canceled
	default:
		return nil
	}
}
