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
	"crypto/rand"
	"math/big"

	"github.com/jackc/pgx/v5"
	"github.com/lib/pq"

	"github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

// QuoteIdentifier returns the identifier safely quoted for use inside a DDL
// statement (schema name, role name, database name, etc.).
func QuoteIdentifier(name string) string {
	return pgx.Identifier{name}.Sanitize()
}

// QuoteLiteral returns the literal value safely quoted for embedding directly
// in DDL.
func QuoteLiteral(value string) string {
	return pq.QuoteLiteral(value)
}

const passwordChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// GeneratePassword generates a cryptographically random password of the given
// length using an alphanumeric character set. The password stays wrapped so it
// never exists as a plaintext string before an approved boundary.
func GeneratePassword(length int) (secret.SecretString, error) {
	chars := make([]byte, length)
	maxInt := big.NewInt(int64(len(passwordChars)))
	for i := range chars {
		n, err := rand.Int(rand.Reader, maxInt)
		if err != nil {
			return secret.SecretString{}, err
		}
		chars[i] = passwordChars[n.Int64()]
	}
	return secret.FromBytes(chars), nil
}
