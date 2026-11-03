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

package secret_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/gomega"
	"go.uber.org/zap/zapcore"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

const (
	plaintext           = "s3cr3t-sentinel-value"
	redacted            = "[REDACTED]"
	visibleHost         = "db.example.test"
	redactionsPerRecord = 2
)

// nestedConfig stands in for a connection config so the tests exercise a
// SecretString reached through a parent struct rather than as a bare value.
type nestedConfig struct {
	Host     string
	Password secret.SecretString
}

func nested() nestedConfig {
	return nestedConfig{Host: visibleHost, Password: secret.FromString(plaintext)}
}

func TestSecretString_ConstructionAndAccessors(t *testing.T) {
	g := NewWithT(t)

	var empty secret.SecretString

	g.Expect(empty.IsZero()).To(BeTrue())
	g.Expect(secret.FromString("").IsZero()).To(BeTrue())
	g.Expect(secret.FromString(plaintext).IsZero()).To(BeFalse())
	g.Expect(secret.FromBytes([]byte(plaintext)).Reveal()).To(Equal(plaintext))
	g.Expect(secret.FromString(plaintext).Reveal()).To(Equal(plaintext))
	g.Expect(secret.FromString(plaintext).String()).To(Equal(redacted))
}

func TestSecretString_FormatsRedacted(t *testing.T) {
	values := []struct {
		name  string
		value any
		// visibleText proves the surrounding value is still rendered, so the
		// redaction assertions cannot pass on empty output.
		visibleText string
	}{
		{name: "direct", value: secret.FromString(plaintext)},
		{name: "nested-in-struct", value: nested(), visibleText: visibleHost},
	}

	formats := []struct {
		name  string
		print func(value any) string
	}{
		{name: "verb-v", print: func(value any) string { return fmt.Sprintf("%v", value) }},
		{name: "verb-s", print: func(value any) string { return fmt.Sprintf("%s", value) }},
		{name: "verb-q", print: func(value any) string { return fmt.Sprintf("%q", value) }},
		{name: "verb-plus-v", print: func(value any) string { return fmt.Sprintf("%+v", value) }},
		{name: "verb-sharp-v", print: func(value any) string { return fmt.Sprintf("%#v", value) }},
		{name: "Sprint", print: func(value any) string { return fmt.Sprint(value) }},
	}

	for _, value := range values {
		for _, format := range formats {
			t.Run(value.name+"-"+format.name, func(t *testing.T) {
				g := NewWithT(t)

				out := format.print(value.value)

				g.Expect(out).To(ContainSubstring(redacted))
				g.Expect(out).NotTo(ContainSubstring(plaintext))
				if value.visibleText != "" {
					g.Expect(out).To(ContainSubstring(value.visibleText))
				}
			})
		}
	}
}

func TestSecretString_MarshalJSONRedacted(t *testing.T) {
	t.Run("bare", func(t *testing.T) {
		g := NewWithT(t)

		out, err := json.Marshal(secret.FromString(plaintext))

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(string(out)).To(Equal(`"[REDACTED]"`))
	})

	t.Run("nested", func(t *testing.T) {
		g := NewWithT(t)

		out, err := json.Marshal(nested())

		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(string(out)).To(Equal(`{"Host":"db.example.test","Password":"[REDACTED]"}`))
	})
}

func TestSecretString_SlogRedacted(t *testing.T) {
	handlers := []struct {
		name    string
		handler func(*bytes.Buffer) slog.Handler
	}{
		{name: "json", handler: func(buf *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(buf, nil) }},
		{name: "text", handler: func(buf *bytes.Buffer) slog.Handler { return slog.NewTextHandler(buf, nil) }},
	}

	for _, h := range handlers {
		t.Run(h.name, func(t *testing.T) {
			g := NewWithT(t)

			var buf bytes.Buffer
			logger := slog.New(h.handler(&buf))

			logger.Info("test", "password", secret.FromString(plaintext), "config", nested())

			out := buf.String()
			g.Expect(strings.Count(out, redacted)).To(Equal(redactionsPerRecord))
			g.Expect(out).To(ContainSubstring(visibleHost))
			g.Expect(out).NotTo(ContainSubstring(plaintext))
		})
	}
}

func TestSecretString_LogrRedacted(t *testing.T) {
	g := NewWithT(t)

	var buf bytes.Buffer
	logger := funcr.New(func(prefix, args string) {
		fmt.Fprint(&buf, prefix, args)
	}, funcr.Options{})

	logger.Info("test", "password", secret.FromString(plaintext), "config", nested())

	out := buf.String()
	g.Expect(strings.Count(out, redacted)).To(Equal(redactionsPerRecord))
	g.Expect(out).To(ContainSubstring(visibleHost))
	g.Expect(out).NotTo(ContainSubstring(plaintext))
}

// TestSecretString_ControllerRuntimeLogStackRedacted is the operator log-stack
// gate. pkg/config/zap.go builds a controller-runtime logr logger over zap and
// selects these encoders, so a SecretString nested in a logged struct must be
// redacted by zap reflection, not only as a top-level value.
func TestSecretString_ControllerRuntimeLogStackRedacted(t *testing.T) {
	encoders := []struct {
		name string
		opt  ctrlzap.Opts
	}{
		{name: "json", opt: ctrlzap.JSONEncoder()},
		{name: "console", opt: ctrlzap.ConsoleEncoder()},
	}

	for _, encoder := range encoders {
		t.Run(encoder.name, func(t *testing.T) {
			g := NewWithT(t)

			var buf bytes.Buffer
			logger := ctrlzap.New(
				ctrlzap.WriteTo(&buf),
				ctrlzap.Level(zapcore.InfoLevel),
				encoder.opt,
			)

			logger.Info("test", "config", nested())

			out := buf.String()
			g.Expect(out).To(ContainSubstring(visibleHost))
			g.Expect(out).To(ContainSubstring(redacted))
			g.Expect(out).NotTo(ContainSubstring(plaintext))
		})
	}
}
