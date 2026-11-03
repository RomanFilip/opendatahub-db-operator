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
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/gomega"
)

func TestSQLInstalledExtensions_ReferencesCatalog(t *testing.T) {
	g := NewWithT(t)
	g.Expect(sqlInstalledExtensions).To(ContainSubstring("pg_extension"))
	g.Expect(sqlInstalledExtensions).To(ContainSubstring("extname"))
	g.Expect(strings.Contains(sqlInstalledExtensions, "ANY($1)")).To(BeTrue())
}

func TestMissingExtensions_Stub(t *testing.T) {
	g := NewWithT(t)
	cli := &extensionStubClient{installedByName: map[string]bool{
		"plpgsql": true,
		"vector":  false,
	}}
	missing, err := MissingExtensions(context.Background(), cli, []string{"plpgsql", "vector"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(missing).To(Equal([]string{"vector"}))
	g.Expect(cli.queryRowCount).To(Equal(1))
	g.Expect(cli.lastSQL).To(ContainSubstring("ANY($1)"))
	g.Expect(cli.lastArgs).To(Equal([]any{[]string{"plpgsql", "vector"}}))
}

func TestMissingExtensions_StubQueryError(t *testing.T) {
	g := NewWithT(t)
	cli := &extensionStubClient{queryErr: errors.New("boom")}
	missing, err := MissingExtensions(context.Background(), cli, []string{"vector"})
	g.Expect(missing).To(BeNil())
	g.Expect(err).To(MatchError(ContainSubstring("checking extension installation")))
	g.Expect(err).To(MatchError(ContainSubstring("boom")))
}

type extensionStubClient struct {
	installedByName map[string]bool
	queryErr        error
	queryRowCount   int
	lastSQL         string
	lastArgs        []any
}

func (c *extensionStubClient) Config() Config { return Config{} }
func (c *extensionStubClient) Close()         {}
func (c *extensionStubClient) Ping(context.Context) error {
	return nil
}
func (c *extensionStubClient) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (c *extensionStubClient) Query(context.Context, string, ...any) (Rows, error) {
	return nil, nil
}
func (c *extensionStubClient) QueryRow(_ context.Context, sql string, args ...any) (pgx.Row, error) {
	c.queryRowCount++
	c.lastSQL = sql
	c.lastArgs = append([]any(nil), args...)
	if c.queryErr != nil {
		return nil, c.queryErr
	}
	if len(args) > 0 {
		if names, ok := args[0].([]string); ok {
			var installed []string
			for _, name := range names {
				if c.installedByName[name] {
					installed = append(installed, name)
				}
			}
			return extensionNamesRow{names: installed}, nil
		}
	}
	return extensionNamesRow{}, nil
}

type extensionNamesRow struct {
	names []string
}

func (r extensionNamesRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("expected one scan destination")
	}
	ptr, ok := dest[0].(*[]string)
	if !ok {
		return errors.New("expected *[]string scan destination")
	}
	*ptr = append([]string(nil), r.names...)
	return nil
}
