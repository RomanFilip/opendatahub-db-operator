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

// Package secret provides a typed wrapper for secret-bearing strings so a
// credential can flow through application structs without a plaintext
// representation on any formatting, logging, or encoding path.
//
// gomega's reflection formatter (format.Object) reads unexported fields and
// bypasses redaction; keep only dummy credentials in Equal-compared Configs in
// tests.
package secret

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// redactedText is the only text a SecretString renders, whatever the format.
const redactedText = "[REDACTED]"

// SecretString holds a secret string. The plaintext is reachable only through
// Reveal; every other path renders redactedText.
type SecretString struct {
	s string
}

// FromBytes wraps raw Secret data.
func FromBytes(data []byte) SecretString {
	return SecretString{s: string(data)}
}

// FromString wraps an already-decoded string.
func FromString(s string) SecretString {
	return SecretString{s: s}
}

// IsZero reports whether the wrapper holds no value. Use it instead of
// comparing the plaintext so callers never need Reveal.
func (s SecretString) IsZero() bool {
	return s.s == ""
}

// Reveal returns the plaintext for the sink that requires it.
//
// Call sites are held to the inventory in allowlist_test.go; adding one needs a
// security review of the sink.
func (s SecretString) Reveal() string {
	return s.s
}

// String implements fmt.Stringer.
func (s SecretString) String() string {
	return redactedText
}

// Format implements fmt.Formatter for every verb, including %#v and %+v.
func (s SecretString) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(redactedText))
}

// LogValue implements slog.LogValuer.
func (s SecretString) LogValue() slog.Value {
	return slog.StringValue(redactedText)
}

// MarshalLog implements logr.Marshaler.
func (s SecretString) MarshalLog() any {
	return redactedText
}

// MarshalJSON implements json.Marshaler, which also redacts SecretString
// nested inside an encoded struct.
func (s SecretString) MarshalJSON() ([]byte, error) {
	return json.Marshal(redactedText)
}
