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
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Client owns a PostgreSQL connection and is the main way to interact with the database.
//
// Every error a Client returns is safe to put in a condition message, an Event,
// or a log line: driver failures come back as *SafeError, which renders no
// credential material while preserving SQLSTATE and retry classification.
type Client interface {
	Config() Config
	Close()
	Ping(ctx context.Context) error
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) (pgx.Row, error)
}

// Rows exposes pgx query results without exposing the underlying connection.
type Rows interface {
	Close()
	Err() error
	CommandTag() pgconn.CommandTag
	FieldDescriptions() []pgconn.FieldDescription
	Next() bool
	Scan(dest ...any) error
	Values() ([]any, error)
	RawValues() [][]byte
}

type pgxClient struct {
	config Config
	pool   *pgxpool.Pool
}

// NewClient opens a long-lived PostgreSQL client with runtime TLS configuration applied.
func NewClient(ctx context.Context, cfg Config) (Client, error) {
	pool, err := openPool(ctx, cfg)
	if err != nil {
		return nil, err
	}

	return &pgxClient{
		config: cfg,
		pool:   pool,
	}, nil
}

// Config returns the connection settings used to open the client.
func (c *pgxClient) Config() Config {
	if c == nil {
		return Config{}
	}

	return c.config
}

// Close closes the owned pool. It is safe to call more than once.
func (c *pgxClient) Close() {
	if c == nil || c.pool == nil {
		return
	}

	c.pool.Close()
	c.pool = nil
}

func (c *pgxClient) Ping(ctx context.Context) error {
	if c == nil || c.pool == nil {
		return fmt.Errorf("postgres client is not open")
	}

	return c.project(ctx, "ping", c.pool.Ping(ctx))
}

func (c *pgxClient) Exec(
	ctx context.Context,
	sql string,
	args ...any,
) (pgconn.CommandTag, error) {
	if c == nil || c.pool == nil {
		return pgconn.CommandTag{}, fmt.Errorf("postgres client is not open")
	}

	tag, err := c.pool.Exec(ctx, sql, args...)
	if err != nil {
		return pgconn.CommandTag{}, c.project(ctx, "executing statement", err)
	}

	return tag, nil
}

func (c *pgxClient) Query(
	ctx context.Context,
	sql string,
	args ...any,
) (Rows, error) {
	if c == nil || c.pool == nil {
		return nil, fmt.Errorf("postgres client is not open")
	}

	rows, err := c.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, c.project(ctx, "starting query", err)
	}

	return safeRows{Rows: rows, secrets: c.boundarySecrets(ctx)}, nil
}

func (c *pgxClient) QueryRow(
	ctx context.Context,
	sql string,
	args ...any,
) (pgx.Row, error) {
	if c == nil || c.pool == nil {
		return nil, fmt.Errorf("postgres client is not open")
	}

	return safeRow{row: c.pool.QueryRow(ctx, sql, args...), secrets: c.boundarySecrets(ctx)}, nil
}

// project converts a driver failure into the shape a Client returns, where the
// raw error, the pool credential, and the statement-scoped secrets are all in
// hand.
func (c *pgxClient) project(ctx context.Context, operation string, err error) error {
	return projectError(operation, err, c.boundarySecrets(ctx))
}

func (c *pgxClient) boundarySecrets(ctx context.Context) boundarySecrets {
	return boundarySecrets{pool: c.config.Password, statement: querySecretsFrom(ctx)}
}

// safeRows projects the errors pgx reports after Query returns. Scan, Values,
// and Err run once the statement's context is gone, so secrets are bound here.
type safeRows struct {
	Rows
	secrets boundarySecrets
}

func (r safeRows) Scan(dest ...any) error {
	return projectError("scanning row", r.Rows.Scan(dest...), r.secrets)
}

func (r safeRows) Err() error {
	return projectError("reading rows", r.Rows.Err(), r.secrets)
}

func (r safeRows) Values() ([]any, error) {
	values, err := r.Rows.Values()
	return values, projectError("reading row values", err, r.secrets)
}

// safeRow projects the error QueryRow defers until Scan.
type safeRow struct {
	row     pgx.Row
	secrets boundarySecrets
}

func (r safeRow) Scan(dest ...any) error {
	return projectError("scanning row", r.row.Scan(dest...), r.secrets)
}

func openPool(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	poolConfig, err := poolConfigFor(cfg)
	if err != nil {
		return nil, projectError("building pool config", err, boundarySecrets{pool: cfg.Password})
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, projectError("opening pool", err, boundarySecrets{pool: cfg.Password})
	}

	return pool, nil
}
