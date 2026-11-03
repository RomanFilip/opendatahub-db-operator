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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/tracelog"
	. "github.com/onsi/gomega"

	"github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

// Sentinels contain a quote and a backslash so QuoteLiteral changes them; a
// redactor that only scrubbed the raw value would leave these in trace output.
const (
	connSentinel  = "conn'ss\\word"
	querySentinel = "role'ss\\word"
)

const queryEvent = "Query"

type capturedTrace struct {
	level tracelog.LogLevel
	msg   string
	data  map[string]any
}

// traceCapture records what the decorator forwards. It stands in for the logr
// sink so tests can assert on the redacted map instead of formatted text.
type traceCapture struct {
	mu     sync.Mutex
	events []capturedTrace
	err    error
	panics bool
}

func (c *traceCapture) sink(_ context.Context, level tracelog.LogLevel, msg string, data map[string]any) error {
	if c.panics {
		panic("sink failure")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, capturedTrace{level: level, msg: msg, data: data})
	return c.err
}

func (c *traceCapture) recorded() []capturedTrace {
	c.mu.Lock()
	defer c.mu.Unlock()
	events := make([]capturedTrace, len(c.events))
	copy(events, c.events)
	return events
}

// newTraceTestLogger pairs the decorator with a capture sink and a buffer that
// records the decorator's own failure logs.
func newTraceTestLogger(password secret.SecretString) (*redactingTraceLogger, *traceCapture, *bytes.Buffer) {
	var failures bytes.Buffer
	logger := funcr.New(func(prefix, args string) {
		fmt.Fprint(&failures, prefix, args)
	}, funcr.Options{})
	capture := &traceCapture{}
	tracer := newRedactingTraceLogger(password, logger)
	tracer.forward = capture.sink
	return tracer, capture, &failures
}

// expectNoSecretMaterial asserts neither the raw nor the QuoteLiteral-escaped
// password appears in rendered trace output.
func expectNoSecretMaterial(t *testing.T, rendered string, password secret.SecretString) {
	t.Helper()
	g := NewWithT(t)
	g.Expect(rendered).NotTo(ContainSubstring(password.Reveal()))
	g.Expect(rendered).NotTo(ContainSubstring(QuoteLiteral(password.Reveal())))
}

func singleEvent(t *testing.T, capture *traceCapture) capturedTrace {
	t.Helper()
	events := capture.recorded()
	NewWithT(t).Expect(events).To(HaveLen(1))
	return events[0]
}

func TestRedactingTraceLogger_RedactsSQLArgsAndKeepsOtherFields(t *testing.T) {
	g := NewWithT(t)
	password := secret.FromString(querySentinel)
	quoted := QuoteLiteral(password.Reveal())
	g.Expect(quoted).NotTo(ContainSubstring(password.Reveal()), "sentinel must require QuoteLiteral escaping")

	tracer, capture, failures := newTraceTestLogger(secret.FromString(connSentinel))
	sql := `ALTER ROLE "role'ss" WITH PASSWORD ` + quoted
	args := []any{"role'ss", password.Reveal(), []byte(password.Reveal()), hex.EncodeToString([]byte(password.Reveal()))}
	data := map[string]any{
		traceFieldSQL:  sql,
		traceFieldArgs: args,
		"commandTag":   "ALTER ROLE",
		traceFieldTime: 5 * time.Millisecond,
		"pid":          uint32(4242),
	}

	tracer.Log(withQuerySecrets(t.Context(), password), tracelog.LogLevelInfo, queryEvent, data)

	event := singleEvent(t, capture)
	g.Expect(event.level).To(Equal(tracelog.LogLevelInfo))
	g.Expect(event.msg).To(Equal(queryEvent))

	emittedSQL, ok := event.data[traceFieldSQL].(string)
	g.Expect(ok).To(BeTrue())
	g.Expect(emittedSQL).To(Equal(`ALTER ROLE "role'ss" WITH PASSWORD ` + redactedValue))
	expectNoSecretMaterial(t, emittedSQL, password)

	emittedArgs, ok := event.data[traceFieldArgs].([]any)
	g.Expect(ok).To(BeTrue())
	g.Expect(emittedArgs).To(HaveLen(4))
	g.Expect(emittedArgs[0]).To(Equal("role'ss"))
	g.Expect(emittedArgs[1]).To(Equal(redactedValue))
	// pgx never hands the decorator a []byte argument; this guards trace data
	// built outside tracelog.logQueryArgs.
	g.Expect(emittedArgs[2]).To(Equal([]byte(redactedValue)))
	// tracelog hex-encodes []byte arguments before the decorator sees them.
	g.Expect(emittedArgs[3]).To(Equal(redactedValue))

	g.Expect(event.data["commandTag"]).To(Equal("ALTER ROLE"))
	g.Expect(event.data[traceFieldTime]).To(Equal(5 * time.Millisecond))
	g.Expect(event.data["pid"]).To(Equal(uint32(4242)))

	// The caller's event belongs to pgx and must survive unchanged.
	g.Expect(data[traceFieldSQL]).To(Equal(sql))
	g.Expect(args[1]).To(Equal(password.Reveal()))
	g.Expect(args[2]).To(Equal([]byte(password.Reveal())))
	g.Expect(failures.String()).To(BeEmpty())
}

// TestRedactingTraceLogger_PassesThroughNilError covers the successful CopyFrom
// event, which tracelog emits at Info level with err: nil.
func TestRedactingTraceLogger_PassesThroughNilError(t *testing.T) {
	g := NewWithT(t)
	tracer, capture, failures := newTraceTestLogger(secret.FromString(connSentinel))

	tracer.Log(t.Context(), tracelog.LogLevelInfo, "CopyFrom", map[string]any{
		"tableName":     pgx.Identifier{"public", "widgets"},
		"columnNames":   []string{"id"},
		traceFieldError: nil,
		"rowCount":      int64(2),
	})

	event := singleEvent(t, capture)
	g.Expect(event.data).To(HaveKey(traceFieldError))
	g.Expect(event.data[traceFieldError]).To(BeNil())
	g.Expect(event.data["rowCount"]).To(Equal(int64(2)))
	g.Expect(fmt.Sprint(event.data)).NotTo(ContainSubstring("withheld"))
	g.Expect(failures.String()).To(BeEmpty())
}

// TestRedactingTraceLogger_RedactsTruncatedArgs covers tracelog's own argument
// rendering: an argument longer than 64 bytes is clipped to its first 64 bytes
// (hex-encoded for []byte), so a long credential reaches the tracer as a prefix
// of itself and would survive matching only complete renderings.
func TestRedactingTraceLogger_RedactsTruncatedArgs(t *testing.T) {
	g := NewWithT(t)
	raw := strings.Repeat("q", 40) + "'\\" + strings.Repeat("z", 40)
	g.Expect(len(raw)).To(BeNumerically(">=", 64), "sentinel must exceed tracelog's 64-byte clip")
	password := secret.FromString(raw)
	truncated := func(rendering string) string {
		return fmt.Sprintf("%s (truncated %d bytes)", rendering[:64], len(rendering)-64)
	}

	tracer, capture, failures := newTraceTestLogger(secret.SecretString{})
	tracer.Log(withQuerySecrets(t.Context(), password), tracelog.LogLevelInfo, queryEvent, map[string]any{
		traceFieldArgs: []any{
			truncated(hex.EncodeToString([]byte(raw))),
			truncated(raw),
		},
	})

	event := singleEvent(t, capture)
	emitted, ok := event.data[traceFieldArgs].([]any)
	g.Expect(ok).To(BeTrue())
	g.Expect(emitted).To(Equal([]any{redactedValue, redactedValue}))
	g.Expect(fmt.Sprint(event.data)).NotTo(ContainSubstring(raw[:64]))
	g.Expect(failures.String()).To(BeEmpty())
}

func TestRedactingTraceLogger_RedactsBoundConnectionPassword(t *testing.T) {
	g := NewWithT(t)
	password := secret.FromString(connSentinel)
	tracer, capture, _ := newTraceTestLogger(password)

	connectErr := fmt.Errorf("failed to connect as %q: password %s rejected", "user", password.Reveal())
	tracer.Log(t.Context(), tracelog.LogLevelError, "Connect", map[string]any{
		traceFieldError: connectErr,
		"host":          "db.example.test",
	})

	event := singleEvent(t, capture)
	g.Expect(event.level).To(Equal(tracelog.LogLevelError))
	g.Expect(event.data["host"]).To(Equal("db.example.test"))
	renderedErr := fmt.Sprint(event.data[traceFieldError])
	g.Expect(renderedErr).To(ContainSubstring(redactedValue))
	expectNoSecretMaterial(t, renderedErr, password)
}

func TestRedactingTraceLogger_RedactsQuotedPasswordInError(t *testing.T) {
	g := NewWithT(t)
	password := secret.FromString(querySentinel)
	tracer, capture, failures := newTraceTestLogger(secret.SecretString{})
	serverErr := fmt.Errorf("syntax error at or near %s", QuoteLiteral(password.Reveal()))

	tracer.Log(withQuerySecrets(t.Context(), password), tracelog.LogLevelError, queryEvent, map[string]any{
		traceFieldError: serverErr,
	})

	event := singleEvent(t, capture)
	rendered := fmt.Sprint(event.data[traceFieldError])
	g.Expect(rendered).To(ContainSubstring("syntax error at or near"))
	g.Expect(rendered).To(ContainSubstring(redactedValue))
	expectNoSecretMaterial(t, rendered, password)
	g.Expect(failures.String()).To(BeEmpty())
}

func TestRedactingTraceLogger_PassesThroughCleanError(t *testing.T) {
	g := NewWithT(t)
	tracer, capture, failures := newTraceTestLogger(secret.FromString(connSentinel))

	pgErr := &pgconn.PgError{Code: "42P01", Message: `relation "missing" does not exist`}
	tracer.Log(t.Context(), tracelog.LogLevelError, queryEvent, map[string]any{
		traceFieldError: pgErr,
		traceFieldSQL:   "SELECT * FROM missing",
	})

	event := singleEvent(t, capture)
	g.Expect(event.data[traceFieldError]).To(BeIdenticalTo(pgErr))
	g.Expect(fmt.Sprint(event.data[traceFieldError])).To(ContainSubstring("does not exist"))
	g.Expect(failures.String()).To(BeEmpty())
}

func TestRedactingTraceLogger_WithholdsUnrecognizedErrorRendering(t *testing.T) {
	password := secret.FromString("Case'Fold\\Sentinel")
	cases := []struct {
		name  string
		shape func(string) string
	}{
		{name: "base64", shape: func(raw string) string { return base64.StdEncoding.EncodeToString([]byte(raw)) }},
		{name: "case-folded", shape: strings.ToLower},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			g := NewWithT(t)
			tracer, capture, failures := newTraceTestLogger(password)
			serverErr := &pgconn.PgError{
				Code:    "28P01",
				Message: "password " + testCase.shape(password.Reveal()) + " rejected",
			}

			tracer.Log(t.Context(), tracelog.LogLevelError, queryEvent, map[string]any{
				traceFieldError: serverErr,
				traceFieldTime:  3 * time.Millisecond,
				"pid":           uint32(7),
			})

			event := singleEvent(t, capture)
			withheld := fmt.Sprint(event.data[traceFieldError])
			g.Expect(withheld).To(ContainSubstring("withheld"))
			g.Expect(withheld).To(ContainSubstring("SQLSTATE 28P01"))
			g.Expect(withheld).To(ContainSubstring("3ms"))
			g.Expect(withheld).NotTo(ContainSubstring(testCase.shape(password.Reveal())))
			expectNoSecretMaterial(t, withheld, password)
			g.Expect(event.data["pid"]).To(Equal(uint32(7)))
			g.Expect(failures.String()).To(BeEmpty())
		})
	}
}

// TestRedactingTraceLogger_QuerySecretsArePerContext runs two statements with
// distinct credentials through one decorator. Each trace must redact its own
// credential; a process-global binding would leak the other goroutine's value.
func TestRedactingTraceLogger_QuerySecretsArePerContext(t *testing.T) {
	g := NewWithT(t)
	type queryCase struct {
		name     string
		msg      string
		password secret.SecretString
	}
	cases := []queryCase{
		{name: "alpha", msg: "QueryAlpha", password: secret.FromString("alpha'one\\pw")},
		{name: "beta", msg: "QueryBeta", password: secret.FromString("beta'two\\pw")},
	}

	tracer, capture, failures := newTraceTestLogger(secret.FromString(connSentinel))
	ctx := t.Context()

	const iterations = 100
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, testCase := range cases {
		wg.Go(func() {
			<-start
			for range iterations {
				sql := "CREATE ROLE " + QuoteIdentifier("role_"+testCase.name) +
					" WITH LOGIN PASSWORD " + QuoteLiteral(testCase.password.Reveal())
				tracer.Log(
					withQuerySecrets(ctx, testCase.password),
					tracelog.LogLevelInfo,
					testCase.msg,
					map[string]any{traceFieldSQL: sql},
				)
			}
		})
	}
	close(start)
	wg.Wait()

	events := capture.recorded()
	g.Expect(events).To(HaveLen(2 * iterations))

	traced := map[string]int{}
	for _, event := range events {
		rendered := fmt.Sprint(event.data)
		switch event.msg {
		case cases[0].msg:
			traced[cases[0].name]++
			expectNoSecretMaterial(t, rendered, cases[0].password)
			g.Expect(rendered).NotTo(ContainSubstring(cases[1].password.Reveal()))
		case cases[1].msg:
			traced[cases[1].name]++
			expectNoSecretMaterial(t, rendered, cases[1].password)
			g.Expect(rendered).NotTo(ContainSubstring(cases[0].password.Reveal()))
		default:
			g.Expect(event.msg).To(BeElementOf(cases[0].msg, cases[1].msg))
		}
	}
	g.Expect(traced).To(Equal(map[string]int{
		cases[0].name: iterations,
		cases[1].name: iterations,
	}))
	g.Expect(failures.String()).To(BeEmpty())
}

type panicError struct{}

func (panicError) Error() string {
	panic("error text failure")
}

func TestRedactingTraceLogger_DropsEventWhenRedactionPanics(t *testing.T) {
	g := NewWithT(t)
	tracer, capture, failures := newTraceTestLogger(secret.FromString(connSentinel))

	g.Expect(func() {
		tracer.Log(t.Context(), tracelog.LogLevelError, queryEvent, map[string]any{
			traceFieldError: panicError{},
			traceFieldSQL:   "SELECT 1",
		})
	}).NotTo(Panic())

	g.Expect(capture.recorded()).To(BeEmpty())
	g.Expect(failures.String()).To(ContainSubstring("dropping pgx trace event"))
	g.Expect(failures.String()).NotTo(ContainSubstring(connSentinel))
}

func TestRedactingTraceLogger_LogsDeliveryFailure(t *testing.T) {
	g := NewWithT(t)
	password := secret.FromString(querySentinel)
	tracer, capture, failures := newTraceTestLogger(secret.FromString(connSentinel))
	capture.err = errors.New("sink unavailable")

	tracer.Log(withQuerySecrets(t.Context(), password), tracelog.LogLevelInfo, queryEvent, map[string]any{
		traceFieldSQL: "ALTER ROLE r WITH PASSWORD " + QuoteLiteral(password.Reveal()),
	})

	events := capture.recorded()
	g.Expect(events).To(HaveLen(1))
	expectNoSecretMaterial(t, fmt.Sprint(events[0].data), password)
	g.Expect(failures.String()).To(ContainSubstring("forwarding pgx trace event"))
}

func TestRedactingTraceLogger_DropsEventWhenSinkPanics(t *testing.T) {
	g := NewWithT(t)
	tracer, capture, failures := newTraceTestLogger(secret.FromString(connSentinel))
	capture.panics = true

	g.Expect(func() {
		tracer.Log(t.Context(), tracelog.LogLevelInfo, queryEvent, map[string]any{traceFieldSQL: "SELECT 1"})
	}).NotTo(Panic())

	g.Expect(capture.recorded()).To(BeEmpty())
	g.Expect(failures.String()).To(ContainSubstring("dropping pgx trace event"))
}

// TestPoolConfigFor_InstallsRedactingTracer exercises the real pgx wiring
// without a server: the pool config carries a standard tracelog.TraceLog whose
// logger redacts the pool's credential, at the level the Config asks for.
func TestPoolConfigFor_InstallsRedactingTracer(t *testing.T) {
	cases := []struct {
		name          string
		traceQueries  bool
		expectedLevel tracelog.LogLevel
	}{
		{name: "errors-only-by-default", traceQueries: false, expectedLevel: tracelog.LogLevelError},
		{name: "every-statement-at-debug", traceQueries: true, expectedLevel: tracelog.LogLevelDebug},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			g := NewWithT(t)
			password := secret.FromString(connSentinel)
			cfg := Config{
				Host:         "db.example.test",
				Port:         5432,
				User:         "user",
				Password:     password,
				DBName:       "db",
				SSLMode:      SSLModeDisable,
				TraceQueries: testCase.traceQueries,
			}

			poolConfig, err := poolConfigFor(cfg)
			g.Expect(err).NotTo(HaveOccurred())
			tracer, ok := poolConfig.ConnConfig.Tracer.(*tracelog.TraceLog)
			g.Expect(ok).To(BeTrue())
			g.Expect(tracer.LogLevel).To(Equal(testCase.expectedLevel))
			decorator, ok := tracer.Logger.(*redactingTraceLogger)
			g.Expect(ok).To(BeTrue())

			capture := &traceCapture{}
			decorator.forward = capture.sink
			tracer.Logger.Log(t.Context(), tracelog.LogLevelError, "Connect", map[string]any{
				traceFieldError: fmt.Errorf("connect failed for password %s", password.Reveal()),
				"host":          cfg.Host,
			})

			event := singleEvent(t, capture)
			g.Expect(fmt.Sprint(event.data[traceFieldError])).To(ContainSubstring(redactedValue))
			g.Expect(event.data["host"]).To(Equal(cfg.Host))
			expectNoSecretMaterial(t, fmt.Sprint(event.data), password)
		})
	}
}

// TestPoolConfigFor_LogsThroughConfiguredLogger pins the injected logger: trace
// output must reach Config.Logger under the tracer's name, not a global one.
func TestPoolConfigFor_LogsThroughConfiguredLogger(t *testing.T) {
	g := NewWithT(t)
	password := secret.FromString(connSentinel)
	var rendered bytes.Buffer
	cfg := Config{
		Host:     "db.example.test",
		Port:     5432,
		User:     "user",
		Password: password,
		DBName:   "db",
		SSLMode:  SSLModeDisable,
		Logger: funcr.New(func(prefix, args string) {
			fmt.Fprint(&rendered, prefix, args)
		}, funcr.Options{}),
	}

	poolConfig, err := poolConfigFor(cfg)
	g.Expect(err).NotTo(HaveOccurred())
	tracer, ok := poolConfig.ConnConfig.Tracer.(*tracelog.TraceLog)
	g.Expect(ok).To(BeTrue())
	tracer.Logger.Log(t.Context(), tracelog.LogLevelError, "Connect", map[string]any{
		traceFieldError: fmt.Errorf("password %s rejected", password.Reveal()),
	})

	g.Expect(rendered.String()).To(ContainSubstring("postgres/tracelog"))
	g.Expect(rendered.String()).To(ContainSubstring(redactedValue))
	expectNoSecretMaterial(t, rendered.String(), password)
}

// recordingExecClient captures what the DDL helpers hand to Exec so a test can
// inspect the statement and the context the pool tracer will receive.
type recordingExecClient struct {
	contexts   []context.Context
	statements []string
}

func (c *recordingExecClient) Config() Config             { return Config{} }
func (c *recordingExecClient) Close()                     {}
func (c *recordingExecClient) Ping(context.Context) error { return nil }

func (c *recordingExecClient) Exec(
	ctx context.Context,
	sql string,
	_ ...any,
) (pgconn.CommandTag, error) {
	c.contexts = append(c.contexts, ctx)
	c.statements = append(c.statements, sql)
	return pgconn.CommandTag{}, nil
}

func (c *recordingExecClient) Query(context.Context, string, ...any) (Rows, error) {
	return nil, nil
}

func (c *recordingExecClient) QueryRow(context.Context, string, ...any) (pgx.Row, error) {
	return nil, nil
}

// TestDDL_PasswordStatementsCarryQuerySecrets pins the contract between the DDL
// helpers and the pool tracer: the statement reaches PostgreSQL with the real
// credential, and the context marks it so the tracer can redact it.
func TestDDL_PasswordStatementsCarryQuerySecrets(t *testing.T) {
	password := secret.FromString(querySentinel)
	cases := []struct {
		name string
		exec func(context.Context, Client) error
	}{
		{
			name: "EnsureRole",
			exec: func(ctx context.Context, cli Client) error {
				return EnsureRole(ctx, cli, "role'ss", password)
			},
		},
		{
			name: "SetRolePassword",
			exec: func(ctx context.Context, cli Client) error {
				return SetRolePassword(ctx, cli, "role'ss", password)
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			g := NewWithT(t)
			cli := &recordingExecClient{}

			g.Expect(testCase.exec(t.Context(), cli)).To(Succeed())
			g.Expect(cli.statements).To(HaveLen(1))
			g.Expect(cli.statements[0]).To(ContainSubstring(QuoteLiteral(password.Reveal())))

			secrets := querySecretsFrom(cli.contexts[0])
			g.Expect(secrets).To(HaveLen(1))
			g.Expect(secrets[0].Reveal()).To(Equal(password.Reveal()))
		})
	}
}
