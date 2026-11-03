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
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/tracelog"

	"github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

// redactedValue is the only text the operator writes in place of a credential.
const redactedValue = "[REDACTED]"

// Trace data keys that carry credential material, SQL text, or the metadata a
// withheld error keeps.
const (
	traceFieldSQL   = "sql"
	traceFieldArgs  = "args"
	traceFieldError = "err"
	traceFieldTime  = "time"
)

// querySecretsKey types the context value that carries the secrets a traced
// statement interpolates as SQL literals. A context value rather than decorator
// state keeps concurrent statements on one pool from seeing each other's
// credentials.
type querySecretsKey struct{}

// withQuerySecrets marks ctx so the pool tracer redacts secrets embedded in
// statements executed under it. pgx passes this context to both trace
// callbacks.
func withQuerySecrets(ctx context.Context, secrets ...secret.SecretString) context.Context {
	stored := make([]secret.SecretString, 0, len(secrets))
	for _, value := range secrets {
		if !value.IsZero() {
			stored = append(stored, value)
		}
	}
	if len(stored) == 0 {
		return ctx
	}
	return context.WithValue(ctx, querySecretsKey{}, stored)
}

func querySecretsFrom(ctx context.Context) []secret.SecretString {
	if ctx == nil {
		return nil
	}
	stored, _ := ctx.Value(querySecretsKey{}).([]secret.SecretString)
	return stored
}

// traceRedactor holds the renderings of one event's known secrets. replace
// holds the forms a trace value can contain verbatim: the raw value and the
// QuoteLiteral form the DDL templates embed. detect also holds case-folded and
// encoded renderings; it is used only to fail closed on error text, so a short
// password cannot corrupt unrelated content through a coincidental encoding.
// encoded holds renderings that are redacted only on an exact argument match,
// which covers tracelog's hex encoding of []byte arguments without rewriting
// unrelated text.
type traceRedactor struct {
	replace []string
	detect  []string
	encoded map[string]struct{}
}

func newTraceRedactor(secrets ...secret.SecretString) traceRedactor {
	redactor := traceRedactor{encoded: map[string]struct{}{}}
	replaced := make(map[string]struct{}, len(secrets)*2)
	detected := make(map[string]struct{}, len(secrets)*9)
	for _, value := range secrets {
		if value.IsZero() {
			continue
		}
		raw := value.Reveal()
		quoted := QuoteLiteral(raw)
		for _, form := range []string{raw, quoted} {
			if _, seen := replaced[form]; seen {
				continue
			}
			replaced[form] = struct{}{}
			redactor.replace = append(redactor.replace, form)
		}
		renderings := []string{
			raw,
			quoted,
			strings.ToLower(raw),
			strings.ToUpper(raw),
			base64.StdEncoding.EncodeToString([]byte(raw)),
			base64.RawStdEncoding.EncodeToString([]byte(raw)),
			base64.URLEncoding.EncodeToString([]byte(raw)),
			base64.RawURLEncoding.EncodeToString([]byte(raw)),
			hex.EncodeToString([]byte(raw)),
		}
		for _, form := range renderings {
			if form == "" {
				continue
			}
			if _, seen := detected[form]; seen {
				continue
			}
			detected[form] = struct{}{}
			redactor.detect = append(redactor.detect, form)
			redactor.encoded[form] = struct{}{}
		}
	}
	// Replace the longest rendering first so an escaped form is never left
	// partially rewritten by a shorter one.
	slices.SortStableFunc(redactor.replace, func(a, b string) int { return len(b) - len(a) })
	return redactor
}

func (r traceRedactor) redactString(value string) string {
	for _, form := range r.replace {
		value = strings.ReplaceAll(value, form, redactedValue)
	}
	return value
}

func (r traceRedactor) redactBytes(value []byte) []byte {
	redacted := value
	for _, form := range r.replace {
		redacted = bytes.ReplaceAll(redacted, []byte(form), []byte(redactedValue))
	}
	return redacted
}

// traceLogger returns the logger for cfg's pool tracer. A Config built without
// one traces nothing rather than reaching for a process-global logger.
func (c Config) traceLogger() logr.Logger {
	logger := c.Logger
	if logger.GetSink() == nil {
		logger = logr.Discard()
	}
	return logger.WithName("postgres").WithName("tracelog")
}

// traceLogLevel reports how much pgx traces for cfg: database errors by
// default, every statement only when the operator asks for that detail.
func (c Config) traceLogLevel() tracelog.LogLevel {
	if c.TraceQueries {
		return tracelog.LogLevelDebug
	}
	return tracelog.LogLevelError
}

func (r traceRedactor) redactArgs(args []any) []any {
	redacted := make([]any, len(args))
	copy(redacted, args)
	for i, arg := range redacted {
		switch value := arg.(type) {
		case string:
			if _, encoded := r.encoded[value]; encoded {
				redacted[i] = redactedValue
				continue
			}
			// tracelog clips long arguments, leaving a prefix of the value it
			// rendered; a clipped credential matches no complete rendering.
			if r.isTruncatedSecret(value) {
				redacted[i] = redactedValue
				continue
			}
			redacted[i] = r.redactString(value)
		case []byte:
			// pgx renders []byte arguments as hex before the tracer sees them
			// (see tracelog.logQueryArgs), so this branch guards trace data
			// assembled outside pgx's own rendering.
			redacted[i] = r.redactBytes(value)
		}
	}
	return redacted
}

// truncatedArgMarker is the suffix tracelog appends to an argument it clipped.
const truncatedArgMarker = " (truncated "

// isTruncatedSecret reports whether value is a clipped rendering that retains a
// prefix of a known secret rendering. tracelog keeps the first 64 bytes of a
// long argument (hex-encoded for []byte), so a credential longer than that
// arrives as a prefix of itself and would survive whole-value matching.
func (r traceRedactor) isTruncatedSecret(value string) bool {
	retained, _, truncated := strings.Cut(value, truncatedArgMarker)
	if !truncated || retained == "" {
		return false
	}
	for _, form := range r.detect {
		if strings.HasPrefix(form, retained) {
			return true
		}
	}
	return false
}

func (r traceRedactor) redactValue(value any) any {
	switch typed := value.(type) {
	case string:
		return r.redactString(typed)
	case []byte:
		return r.redactBytes(typed)
	case []any:
		return r.redactArgs(typed)
	case error:
		return errors.New(r.redactString(typed.Error()))
	default:
		return value
	}
}

// redactError scrubs a failed event's error text. When the replacement cannot
// account for every known rendering the whole message is withheld in favor of
// safe metadata: a leak is worse than losing diagnostics.
func (r traceRedactor) redactError(operation string, value any, data map[string]any) any {
	message, ok := traceErrorMessage(value)
	if !ok {
		return safeTraceError(operation, nil, data)
	}
	redacted := r.redactString(message)
	if r.containsSecret(redacted) {
		return safeTraceError(operation, value, data)
	}
	if redacted == message {
		return value
	}
	if _, isError := value.(error); isError {
		return errors.New(redacted)
	}
	return redacted
}

func (r traceRedactor) containsSecret(value string) bool {
	for _, form := range r.detect {
		if strings.Contains(value, form) {
			return true
		}
	}
	return false
}

func traceErrorMessage(value any) (string, bool) {
	switch typed := value.(type) {
	case error:
		return typed.Error(), true
	case string:
		return typed, true
	default:
		return "", false
	}
}

// safeTraceError builds the error logged in place of unredactable driver text.
// It carries only the operation, the server's SQLSTATE, and the duration.
func safeTraceError(operation string, cause any, data map[string]any) error {
	metadata := make([]string, 0, 2)
	var pgErr *pgconn.PgError
	if err, ok := cause.(error); ok && err != nil && errors.As(err, &pgErr) && pgErr.Code != "" {
		metadata = append(metadata, "SQLSTATE "+pgErr.Code)
	}
	if duration, ok := data[traceFieldTime].(time.Duration); ok {
		metadata = append(metadata, duration.String())
	}
	if len(metadata) == 0 {
		return fmt.Errorf("%s failed: error text withheld", operation)
	}
	return fmt.Errorf("%s failed: error text withheld (%s)", operation, strings.Join(metadata, ", "))
}

// traceSink delivers an already-redacted trace event. The error return lets the
// decorator treat a delivery failure as an internal failure instead of
// forwarding anything further.
type traceSink func(ctx context.Context, level tracelog.LogLevel, msg string, data map[string]any) error

// redactingTraceLogger is the operator-owned tracelog.Logger for one pool. It
// binds the pool's connection credential at construction and reads
// statement-scoped secrets from the event context, so no credential state is
// process-global.
type redactingTraceLogger struct {
	// logger reports decorator failures and receives no trace data.
	logger logr.Logger
	// forward emits the redacted event.
	forward traceSink
	// password is the pool's connection credential, which pgx can echo in
	// connection errors.
	password secret.SecretString
}

func newRedactingTraceLogger(password secret.SecretString, logger logr.Logger) *redactingTraceLogger {
	return &redactingTraceLogger{
		logger:   logger,
		forward:  logrTraceSink(logger),
		password: password,
	}
}

// Log implements tracelog.Logger. Any internal failure drops the event: a trace
// line is diagnostic, so losing it is preferable to forwarding unredacted data.
func (l *redactingTraceLogger) Log(ctx context.Context, level tracelog.LogLevel, msg string, data map[string]any) {
	defer func() {
		if recovered := recover(); recovered != nil {
			l.logger.Error(
				fmt.Errorf("pgx trace event dropped after %T panic", recovered),
				"dropping pgx trace event",
				"event", msg,
			)
		}
	}()

	redacted := l.redactEvent(ctx, msg, data)
	if err := l.forward(ctx, level, msg, redacted); err != nil {
		l.logger.Error(err, "forwarding pgx trace event", "event", msg)
	}
}

func (l *redactingTraceLogger) redactEvent(
	ctx context.Context,
	operation string,
	data map[string]any,
) map[string]any {
	querySecrets := querySecretsFrom(ctx)
	secrets := make([]secret.SecretString, 0, 1+len(querySecrets))
	secrets = append(secrets, l.password)
	secrets = append(secrets, querySecrets...)
	redactor := newTraceRedactor(secrets...)

	// Copy before mutating: the caller's map belongs to pgx, and a partial
	// rewrite would corrupt the event for any other consumer.
	redacted := make(map[string]any, len(data))
	for key, value := range data {
		if key != traceFieldError {
			redacted[key] = redactor.redactValue(value)
			continue
		}
		// pgx reports err: nil on successful events (CopyFrom), where inventing
		// a withheld failure would misreport the statement.
		if value == nil {
			redacted[key] = nil
			continue
		}
		redacted[key] = redactor.redactError(operation, value, data)
	}
	return redacted
}

// logrTraceSink maps pgx levels onto logr, which has no warn level: errors keep
// the error call, while warnings stay at the ordinary level.
func logrTraceSink(logger logr.Logger) traceSink {
	return func(_ context.Context, level tracelog.LogLevel, msg string, data map[string]any) error {
		switch level {
		case tracelog.LogLevelError:
			fields, cause := traceErrorFields(data)
			logger.Error(cause, msg, fields...)
		case tracelog.LogLevelWarn, tracelog.LogLevelInfo:
			logger.Info(msg, traceFields(data)...)
		case tracelog.LogLevelDebug:
			logger.V(1).Info(msg, traceFields(data)...)
		default:
			logger.V(2).Info(msg, traceFields(data)...)
		}
		return nil
	}
}

// traceErrorFields supplies the non-nil cause logr's Error needs and omits the
// same error from the structured fields.
func traceErrorFields(data map[string]any) ([]any, error) {
	if err, ok := data[traceFieldError].(error); ok && err != nil {
		return traceFields(data, traceFieldError), err
	}
	return traceFields(data), errors.New("pgx trace event reported an error")
}

// traceFields flattens event data into logr key/value pairs in a stable order.
func traceFields(data map[string]any, skip ...string) []any {
	omitted := make(map[string]struct{}, len(skip))
	for _, key := range skip {
		omitted[key] = struct{}{}
	}
	keys := make([]string, 0, len(data))
	for key := range data {
		if _, skipped := omitted[key]; skipped {
			continue
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	fields := make([]any, 0, 2*len(keys))
	for _, key := range keys {
		fields = append(fields, key, data[key])
	}
	return fields
}
