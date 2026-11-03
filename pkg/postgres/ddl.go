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
	"database/sql"
	"fmt"
	"strings"

	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

// SQL statement templates. Each uses %s placeholders for already-quoted
// identifiers/literals -- never for raw strings. Placing them as named
// constants keeps the DDL readable and makes the quoting contract explicit:
// every %s argument must come from QuoteIdentifier or QuoteLiteral.
const (
	sqlCreateSchemaIfNotExists = "CREATE SCHEMA IF NOT EXISTS %s"
	sqlCreateDatabase          = "CREATE DATABASE %s"
	sqlDropDatabase            = "DROP DATABASE IF EXISTS %s"
	sqlDropDatabaseForce       = "DROP DATABASE IF EXISTS %s WITH (FORCE)"
	sqlDropSchemaCascade       = "DROP SCHEMA IF EXISTS %s CASCADE"
	sqlDropRole                = "DROP ROLE IF EXISTS %s"
	// Terminate backends for a login role or database without killing the
	// caller's own session. Bound parameters — never fmt into these.
	sqlTerminateRoleSessions = "" +
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity " +
		"WHERE usename = $1 AND pid <> pg_backend_pid()"
	sqlTerminateDatabaseSessions = "" +
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity " +
		"WHERE datname = $1 AND pid <> pg_backend_pid()"
	sqlReassignOwned       = "REASSIGN OWNED BY %s TO %s"
	sqlDropOwned           = "DROP OWNED BY %s"
	sqlGrantUsageOnSchema  = "GRANT USAGE ON SCHEMA %s TO %s"
	sqlGrantCreateOnSchema = "GRANT CREATE ON SCHEMA %s TO %s"

	sqlGrantSelectOnTables    = "GRANT SELECT ON ALL TABLES IN SCHEMA %s TO %s"
	sqlDefaultPrivGrantSelect = "ALTER DEFAULT PRIVILEGES IN SCHEMA %s GRANT SELECT ON TABLES TO %s"
	sqlGrantDMLOnTables       = "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %s TO %s"
	sqlDefaultPrivGrantDML    = "" +
		"ALTER DEFAULT PRIVILEGES IN SCHEMA %s " +
		"GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s"
	// ReadOnly gets SELECT only: USAGE would allow nextval and advance the
	// counter, which is a write.
	sqlGrantSelectOnSequences            = "GRANT SELECT ON ALL SEQUENCES IN SCHEMA %s TO %s"
	sqlDefaultPrivGrantSelectOnSequences = "" +
		"ALTER DEFAULT PRIVILEGES IN SCHEMA %s GRANT SELECT ON SEQUENCES TO %s"
	sqlGrantDMLOnSequences            = "GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA %s TO %s"
	sqlDefaultPrivGrantDMLOnSequences = "" +
		"ALTER DEFAULT PRIVILEGES IN SCHEMA %s " +
		"GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO %s"

	sqlGrantConnectOnDatabase = "GRANT CONNECT ON DATABASE %s TO %s"
	sqlGrantCreateOnDatabase  = "GRANT CREATE ON DATABASE %s TO %s"

	sqlCreateExtensionIfNotExists = "CREATE EXTENSION IF NOT EXISTS %s"

	// Installed in the connected database (not merely loadable on the instance).
	sqlInstalledExtensions = "SELECT ARRAY(SELECT extname FROM pg_extension WHERE extname = ANY($1))"

	sqlDatabaseExists     = "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)"
	sqlSchemaExists       = "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname = $1)"
	sqlSchemaOwnerComment = "" +
		"SELECT obj_description(n.oid, 'pg_namespace') " +
		"FROM pg_namespace n WHERE n.nspname = $1"
	sqlSetRolePassword = `ALTER ROLE %s WITH PASSWORD %s`

	// schemaOwnerMarkerPrefix ties a schema to the SchemaClaim UID that created
	// it. Written in the same statement as CREATE SCHEMA so recovery can prove
	// ownership without trusting Kubernetes peer status alone.
	schemaOwnerMarkerPrefix = "opendatahub.io/schemaclaim-uid="
)

// sqlEnsureRole is a PL/pgSQL DO block that creates a role only when it does
// not already exist. Placeholders: 1) role-as-literal (pg_roles lookup),
// 2) role-as-identifier + password-as-literal (CREATE branch only).
const sqlEnsureRole = `DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN
    CREATE ROLE %s WITH LOGIN PASSWORD %s;
  END IF;
END
$$`

// CreateSchema creates a schema if it doesn't already exist. Idempotent.
func CreateSchema(ctx context.Context, cli Client, schema string) error {
	_, err := cli.Exec(ctx, fmt.Sprintf(sqlCreateSchemaIfNotExists, QuoteIdentifier(schema)))
	return err
}

func schemaOwnerMarker(claimUID string) string {
	return schemaOwnerMarkerPrefix + claimUID
}

// CreateSchemaExclusiveWithOwner creates a schema and records claimUID as its
// ownership marker in one Exec (implicit transaction on one pooled connection).
// Returns a duplicate-schema error if the schema already exists.
func CreateSchemaExclusiveWithOwner(ctx context.Context, cli Client, schema, claimUID string) error {
	if claimUID == "" {
		return fmt.Errorf("claim UID is required to mark schema ownership")
	}
	// %%I / %%L survive fmt.Sprintf and become %I / %L for format().
	stmt := fmt.Sprintf(`DO $odh$
BEGIN
  EXECUTE format('CREATE SCHEMA %%I', %s);
  EXECUTE format('COMMENT ON SCHEMA %%I IS %%L', %s, %s);
END
$odh$`, QuoteLiteral(schema), QuoteLiteral(schema), QuoteLiteral(schemaOwnerMarker(claimUID)))
	_, err := cli.Exec(ctx, stmt)
	return err
}

// SchemaOwnerClaimUID returns the SchemaClaim UID stored on schema, or "" when
// the schema has no operator ownership marker.
func SchemaOwnerClaimUID(ctx context.Context, cli Client, schema string) (string, error) {
	row, err := cli.QueryRow(ctx, sqlSchemaOwnerComment, schema)
	if err != nil {
		return "", err
	}
	var comment sql.NullString
	if err := row.Scan(&comment); err != nil {
		return "", err
	}
	if !comment.Valid || !strings.HasPrefix(comment.String, schemaOwnerMarkerPrefix) {
		return "", nil
	}
	return strings.TrimPrefix(comment.String, schemaOwnerMarkerPrefix), nil
}

// CreateDatabase creates a database if it doesn't already exist. Idempotent.
// Concurrent creates that lose the race on DuplicateDatabase are treated as success.
func CreateDatabase(ctx context.Context, cli Client, database string) error {
	exists, err := DatabaseExists(ctx, cli, database)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = cli.Exec(ctx, fmt.Sprintf(sqlCreateDatabase, QuoteIdentifier(database)))
	if err != nil && IsDuplicateDatabase(err) {
		return nil
	}
	return err
}

// DropDatabase drops a database if it exists. Idempotent. Callers must never
// use this for a provider's shared default database.
//
// On PostgreSQL 13+ it uses WITH (FORCE) so claim DeletionPolicy=Delete is not
// blocked by leftover application sessions. Older servers terminate other
// backends connected to the database, then issue a plain DROP DATABASE.
func DropDatabase(ctx context.Context, cli Client, database string) error {
	version, err := ServerVersion(ctx, cli)
	if err != nil {
		return fmt.Errorf("reading server version before dropping database %q: %w", database, err)
	}
	if version.Major >= 13 {
		return dropDatabase(ctx, cli, fmt.Sprintf(sqlDropDatabaseForce, QuoteIdentifier(database)))
	}
	if err := TerminateDatabaseSessions(ctx, cli, database); err != nil {
		return fmt.Errorf("terminating sessions for database %q: %w", database, err)
	}
	return dropDatabase(ctx, cli, fmt.Sprintf(sqlDropDatabase, QuoteIdentifier(database)))
}

func dropDatabase(ctx context.Context, cli Client, stmt string) error {
	_, err := cli.Exec(ctx, stmt)
	if err != nil && (IsInvalidCatalogName(err) || IsUndefinedObject(err)) {
		return nil
	}
	return err
}

// DropSchemaCascade drops a schema and all its contained objects. Used for
// DeletionPolicy=Delete (docs/plan.md §6).
func DropSchemaCascade(ctx context.Context, cli Client, schema string) error {
	_, err := cli.Exec(ctx, fmt.Sprintf(sqlDropSchemaCascade, QuoteIdentifier(schema)))
	return err
}

// EnsureRole creates a PostgreSQL login role with the given password if it does
// not already exist. It is a true CREATE-only idempotent operation — it never
// alters an existing role's password. Use SetRolePassword for explicit rotation.
// Concurrent creates that lose the race on DuplicateObject are treated as success.
func EnsureRole(ctx context.Context, cli Client, role string, password secret.SecretString) error {
	// The statement holds the password as a literal: the pool tracer redacts it
	// from traces, and Client error projection redacts it from driver text that
	// quotes the statement back.
	ctx = withQuerySecrets(ctx, password)
	stmt := fmt.Sprintf(sqlEnsureRole,
		QuoteLiteral(role),
		QuoteIdentifier(role), QuoteLiteral(password.Reveal()),
	)
	_, err := cli.Exec(ctx, stmt)
	if err != nil && IsDuplicateObject(err) {
		return nil
	}
	return err
}

// SetRolePassword explicitly rotates the password of an existing PostgreSQL role.
// Callers must ensure this is intentional — rotating breaks all active connections
// that use the old credentials. Only call this when a credentials Secret has been
// lost and an explicit rotation is required to restore a known-good state.
func SetRolePassword(ctx context.Context, cli Client, role string, password secret.SecretString) error {
	// Same two consumers as EnsureRole: trace rendering and error projection.
	ctx = withQuerySecrets(ctx, password)
	_, err := cli.Exec(ctx, fmt.Sprintf(sqlSetRolePassword, QuoteIdentifier(role), QuoteLiteral(password.Reveal())))
	return err
}

// DropRole drops a PostgreSQL role if it exists. Idempotent.
//
// Active sessions for the role are terminated first so DROP ROLE is not blocked
// by claim consumers that still hold connections after deletion starts.
func DropRole(ctx context.Context, cli Client, role string) error {
	if err := TerminateRoleSessions(ctx, cli, role); err != nil {
		return fmt.Errorf("terminating sessions for role %q: %w", role, err)
	}
	_, err := cli.Exec(ctx, fmt.Sprintf(sqlDropRole, QuoteIdentifier(role)))
	return err
}

// TerminateRoleSessions ends backends authenticated as role, excluding the
// caller's session. Idempotent when the role has no open sessions.
func TerminateRoleSessions(ctx context.Context, cli Client, role string) error {
	_, err := cli.Exec(ctx, sqlTerminateRoleSessions, role)
	return err
}

// TerminateDatabaseSessions ends backends connected to database, excluding the
// caller's session. Used when DROP DATABASE ... WITH (FORCE) is unavailable.
func TerminateDatabaseSessions(ctx context.Context, cli Client, database string) error {
	_, err := cli.Exec(ctx, sqlTerminateDatabaseSessions, database)
	return err
}

// ReassignOwned transfers objects owned by role to newOwner. Must run while
// connected to the database that contains those objects.
func ReassignOwned(ctx context.Context, cli Client, role, newOwner string) error {
	_, err := cli.Exec(ctx, fmt.Sprintf(sqlReassignOwned, QuoteIdentifier(role), QuoteIdentifier(newOwner)))
	return err
}

// DropOwned removes privileges and default privileges owned by role in the
// current database. Call after ReassignOwned so object ownership is cleared
// first; together they allow DropRole to succeed when the role created objects.
func DropOwned(ctx context.Context, cli Client, role string) error {
	_, err := cli.Exec(ctx, fmt.Sprintf(sqlDropOwned, QuoteIdentifier(role)))
	return err
}

// ClearRoleOwnedObjects reassigns then drops ownership for role in the current
// database so DropRole is not blocked by dependent objects (SQLSTATE 2BP01).
func ClearRoleOwnedObjects(ctx context.Context, cli Client, role, newOwner string) error {
	if err := ReassignOwned(ctx, cli, role, newOwner); err != nil {
		return fmt.Errorf("reassigning owned objects from %q to %q: %w", role, newOwner, err)
	}
	if err := DropOwned(ctx, cli, role); err != nil {
		return fmt.Errorf("dropping owned privileges for %q: %w", role, err)
	}
	return nil
}

// GrantSchemaPrivileges grants privileges to a role on a schema according to
// the claim access mode. ReadWrite grants schema-local administration: USAGE,
// CREATE, and DML on tables and sequences. ReadOnly grants schema USAGE, table
// SELECT, and sequence SELECT only (no sequence USAGE, so nextval is denied).
// It also sets default privileges so future admin-created objects are covered
// (including serial/identity sequences).
func GrantSchemaPrivileges(
	ctx context.Context,
	cli Client,
	schema string,
	role string,
	accessMode infraApi.AccessMode,
) error {
	q := QuoteIdentifier

	if _, err := cli.Exec(ctx, fmt.Sprintf(sqlGrantUsageOnSchema, q(schema), q(role))); err != nil {
		return fmt.Errorf("grant usage on schema: %w", err)
	}

	var tableGrant, defaultPriv, seqGrant, defaultSeqPriv string
	if accessMode == infraApi.AccessModeReadOnly {
		tableGrant = sqlGrantSelectOnTables
		defaultPriv = sqlDefaultPrivGrantSelect
		seqGrant = sqlGrantSelectOnSequences
		defaultSeqPriv = sqlDefaultPrivGrantSelectOnSequences
	} else {
		if _, err := cli.Exec(ctx, fmt.Sprintf(sqlGrantCreateOnSchema, q(schema), q(role))); err != nil {
			return fmt.Errorf("grant create on schema: %w", err)
		}
		tableGrant = sqlGrantDMLOnTables
		defaultPriv = sqlDefaultPrivGrantDML
		seqGrant = sqlGrantDMLOnSequences
		defaultSeqPriv = sqlDefaultPrivGrantDMLOnSequences
	}

	if _, err := cli.Exec(ctx, fmt.Sprintf(tableGrant, q(schema), q(role))); err != nil {
		return fmt.Errorf("grant on tables: %w", err)
	}
	if _, err := cli.Exec(ctx, fmt.Sprintf(defaultPriv, q(schema), q(role))); err != nil {
		return fmt.Errorf("alter default privileges on tables: %w", err)
	}
	if _, err := cli.Exec(ctx, fmt.Sprintf(seqGrant, q(schema), q(role))); err != nil {
		return fmt.Errorf("grant on sequences: %w", err)
	}
	if _, err := cli.Exec(ctx, fmt.Sprintf(defaultSeqPriv, q(schema), q(role))); err != nil {
		return fmt.Errorf("alter default privileges on sequences: %w", err)
	}
	return nil
}

// GrantDatabasePrivileges grants privileges on a database according to the
// claim access mode. ReadOnly grants CONNECT only; ReadWrite grants both
// CONNECT and CREATE.
func GrantDatabasePrivileges(
	ctx context.Context,
	cli Client,
	database string,
	role string,
	accessMode infraApi.AccessMode,
) error {
	q := QuoteIdentifier
	if _, err := cli.Exec(ctx, fmt.Sprintf(sqlGrantConnectOnDatabase, q(database), q(role))); err != nil {
		return fmt.Errorf("grant connect on database: %w", err)
	}
	if accessMode != infraApi.AccessModeReadOnly {
		if _, err := cli.Exec(ctx, fmt.Sprintf(sqlGrantCreateOnDatabase, q(database), q(role))); err != nil {
			return fmt.Errorf("grant create on database: %w", err)
		}
	}
	return nil
}

// RevokeDatabasePrivileges revokes the privileges granted by GrantDatabasePrivileges.
// Missing roles (42704) and missing databases (3D000) are treated as already gone.
func RevokeDatabasePrivileges(ctx context.Context, cli Client, database, role string) error {
	q := QuoteIdentifier
	var errs []error
	if _, err := cli.Exec(ctx, "REVOKE CREATE ON DATABASE "+q(database)+" FROM "+q(role)); err != nil &&
		!IsUndefinedObject(err) && !IsInvalidCatalogName(err) {
		errs = append(errs, err)
	}
	if _, err := cli.Exec(ctx, "REVOKE CONNECT ON DATABASE "+q(database)+" FROM "+q(role)); err != nil &&
		!IsUndefinedObject(err) && !IsInvalidCatalogName(err) {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("revoking database privileges (partial): %v", errs)
}

// RevokeSchemaPrivileges revokes the privileges granted by GrantSchemaPrivileges.
// Each revocation is attempted in order; errors are joined and returned so the
// caller is aware of partial failures, but revocation continues even if one
// step fails (the role or schema may already be gone during deletion).
func RevokeSchemaPrivileges(ctx context.Context, cli Client, schema, role string) error {
	q := QuoteIdentifier
	var errs []error

	if _, err := cli.Exec(
		ctx,
		"ALTER DEFAULT PRIVILEGES IN SCHEMA "+q(schema)+" REVOKE ALL ON SEQUENCES FROM "+q(role),
	); err != nil && !IsUndefinedObject(err) {
		errs = append(errs, err)
	}

	if _, err := cli.Exec(
		ctx,
		"REVOKE ALL ON ALL SEQUENCES IN SCHEMA "+q(schema)+" FROM "+q(role),
	); err != nil && !IsUndefinedObject(err) {
		errs = append(errs, err)
	}

	if _, err := cli.Exec(
		ctx,
		"ALTER DEFAULT PRIVILEGES IN SCHEMA "+q(schema)+" REVOKE ALL ON TABLES FROM "+q(role),
	); err != nil && !IsUndefinedObject(err) {
		errs = append(errs, err)
	}

	if _, err := cli.Exec(
		ctx,
		"REVOKE ALL ON ALL TABLES IN SCHEMA "+q(schema)+" FROM "+q(role),
	); err != nil && !IsUndefinedObject(err) {
		errs = append(errs, err)
	}

	if _, err := cli.Exec(
		ctx,
		"REVOKE ALL ON SCHEMA "+q(schema)+" FROM "+q(role),
	); err != nil && !IsUndefinedObject(err) {
		errs = append(errs, err)
	}

	if len(errs) == 0 {
		return nil
	}

	return fmt.Errorf("revoking schema privileges (partial): %v", errs)
}

// HasSchemaPrivileges reports whether role already holds schema-level
// privileges for accessMode: USAGE for ReadOnly, USAGE+CREATE for ReadWrite.
// COALESCE treats NULL from an unknown role as false.
//
// This is a shallow probe (schema USAGE/CREATE only). It does not cover table,
// sequence, or default-privilege grants that GrantSchemaPrivileges also
// applies. SchemaClaim reconcile therefore re-runs GrantSchemaPrivileges
// every time instead of gating on this check.
func HasSchemaPrivileges(
	ctx context.Context,
	cli Client,
	schema string,
	role string,
	accessMode infraApi.AccessMode,
) (bool, error) {
	row, err := cli.QueryRow(ctx,
		"SELECT COALESCE(HAS_SCHEMA_PRIVILEGE($1, $2, 'USAGE'), false)", role, schema,
	)
	if err != nil {
		return false, err
	}

	var hasUsage bool
	err = row.Scan(&hasUsage)
	if err != nil || !hasUsage {
		return false, err
	}
	if accessMode == infraApi.AccessModeReadOnly {
		return true, nil
	}

	row, err = cli.QueryRow(ctx,
		"SELECT COALESCE(HAS_SCHEMA_PRIVILEGE($1, $2, 'CREATE'), false)", role, schema,
	)
	if err != nil {
		return false, err
	}

	var hasCreate bool
	err = row.Scan(&hasCreate)

	return hasCreate, err
}

// HasDatabasePrivileges reports whether role already holds the privileges
// expected for accessMode on database: CONNECT for ReadOnly, CONNECT+CREATE
// for ReadWrite.
func HasDatabasePrivileges(
	ctx context.Context,
	cli Client,
	database string,
	role string,
	accessMode infraApi.AccessMode,
) (bool, error) {
	row, err := cli.QueryRow(ctx,
		"SELECT COALESCE(HAS_DATABASE_PRIVILEGE($1, $2, 'CONNECT'), false)", role, database,
	)
	if err != nil {
		return false, err
	}

	var hasConnect bool
	err = row.Scan(&hasConnect)
	if err != nil || !hasConnect {
		return false, err
	}
	if accessMode == infraApi.AccessModeReadOnly {
		return true, nil
	}
	row, err = cli.QueryRow(ctx,
		"SELECT COALESCE(HAS_DATABASE_PRIVILEGE($1, $2, 'CREATE'), false)", role, database,
	)
	if err != nil {
		return false, err
	}

	var hasCreate bool
	err = row.Scan(&hasCreate)
	return hasCreate, err
}

// DatabaseExists returns true if a database with the given name exists on the
// server. Used by DatabaseClaim to verify spec.database before provisioning.
func DatabaseExists(ctx context.Context, cli Client, name string) (bool, error) {
	row, err := cli.QueryRow(ctx, sqlDatabaseExists, name)
	if err != nil {
		return false, err
	}

	var exists bool
	err = row.Scan(&exists)
	return exists, err
}

func SchemaExists(ctx context.Context, cli Client, name string) (bool, error) {
	row, err := cli.QueryRow(ctx, sqlSchemaExists, name)
	if err != nil {
		return false, err
	}

	var exists bool
	err = row.Scan(&exists)
	return exists, err
}

func RoleExists(ctx context.Context, cli Client, name string) (bool, error) {
	row, err := cli.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = $1)", name,
	)
	if err != nil {
		return false, err
	}

	var exists bool
	err = row.Scan(&exists)
	return exists, err
}

// CreateExtensionIfNotExists runs CREATE EXTENSION IF NOT EXISTS for each name.
// Used by the Embedded provider's bootstrap step (task-08).
func CreateExtensionIfNotExists(ctx context.Context, cli Client, extensions []string) error {
	for _, ext := range extensions {
		if _, err := cli.Exec(ctx, fmt.Sprintf(sqlCreateExtensionIfNotExists, QuoteIdentifier(ext))); err != nil {
			return fmt.Errorf("creating extension %q: %w", ext, err)
		}
	}

	return nil
}

// MissingExtensions returns the subset of names absent from pg_extension in
// the connected database, in input order. Empty input yields a nil slice.
// One round-trip covers the whole list.
func MissingExtensions(ctx context.Context, cli Client, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}

	row, err := cli.QueryRow(ctx, sqlInstalledExtensions, names)
	if err != nil {
		return nil, fmt.Errorf("checking extension installation: %w", err)
	}

	var installed []string
	if err := row.Scan(&installed); err != nil {
		return nil, fmt.Errorf("checking extension installation: %w", err)
	}

	have := make(map[string]struct{}, len(installed))
	for _, name := range installed {
		have[name] = struct{}{}
	}

	var missing []string
	for _, name := range names {
		if _, ok := have[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing, nil
}
