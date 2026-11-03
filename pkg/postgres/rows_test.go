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
	"errors"
	"fmt"
	"reflect"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

type valuesErrorRows struct {
	Rows
	values []any
	err    error
}

func (r valuesErrorRows) Values() ([]any, error) {
	return r.values, r.err
}

func TestSafeRows_ValuesProjectsError(t *testing.T) {
	g := NewWithT(t)
	password := secret.FromString(`values'password\sentinel`)
	rawErr := fmt.Errorf("cannot decode value %s", QuoteLiteral(password.Reveal()))
	partialValues := []any{"partial result"}
	rows := safeRows{
		Rows:    valuesErrorRows{values: partialValues, err: rawErr},
		secrets: boundarySecrets{statement: []secret.SecretString{password}},
	}

	values, err := rows.Values()

	g.Expect(values).To(Equal(partialValues))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err).NotTo(BeIdenticalTo(rawErr))
	g.Expect(err.Error()).To(ContainSubstring("reading row values failed"))
	g.Expect(err.Error()).To(ContainSubstring("[REDACTED]"))
	g.Expect(err.Error()).NotTo(ContainSubstring(password.Reveal()))

	var safeErr *SafeError
	g.Expect(errors.As(err, &safeErr)).To(BeTrue())
}

func TestSafeRows_ValuesPreservesSuccess(t *testing.T) {
	g := NewWithT(t)
	values := []any{"decoded value"}
	rows := safeRows{Rows: valuesErrorRows{values: values}}

	actual, err := rows.Values()

	g.Expect(actual).To(Equal(values))
	g.Expect(err).NotTo(HaveOccurred())
}

func TestRows_InterfaceExcludesConnection(t *testing.T) {
	g := NewWithT(t)
	rowsType := reflect.TypeFor[Rows]()
	clientType := reflect.TypeFor[Client]()
	queryMethod, hasQuery := clientType.MethodByName("Query")
	g.Expect(hasQuery).To(BeTrue())
	g.Expect(queryMethod.Type.Out(0)).To(Equal(rowsType))

	_, hasConnection := rowsType.MethodByName("Conn")
	g.Expect(hasConnection).To(BeFalse())
}
