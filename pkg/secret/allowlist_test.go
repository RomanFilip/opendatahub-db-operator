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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
)

// revealMethod is the plaintext accessor this inventory exists to contain.
const revealMethod = "Reveal"

// exampleSource is the stand-in path shared by the scanner cases.
const exampleSource = "pkg/example/example.go"

// sinkSite is the stand-in allowlisted site shared by the diff cases.
var sinkSite = revealSite{file: exampleSource, function: "sink"}

// packageScopeSite labels a reveal outside every function body, where no
// enclosing function can carry the allowlist decision.
const packageScopeSite = "<package scope>"

// revealSite identifies one plaintext access by source file and enclosing
// function.
type revealSite struct {
	file     string
	function string
}

func (s revealSite) String() string {
	function := s.function
	if function == "" {
		function = packageScopeSite
	}
	return s.file + ":" + function
}

// allowedRevealSites is the complete inventory of places where a wrapped
// credential becomes plaintext. Every entry is a reviewed egress boundary; a
// change to this list is a security decision, not a refactor.
var allowedRevealSites = []revealSite{
	// pgx holds the pool credential as a plain string in ConnConfig.
	{file: "pkg/postgres/client_tls.go", function: "poolConfigFor"},
	// DDL embeds the role password as a SQL literal that the tracer scrubs.
	{file: "pkg/postgres/ddl.go", function: "EnsureRole"},
	// SetRolePassword writes the same scrubbed DDL literal sink.
	{file: "pkg/postgres/ddl.go", function: "SetRolePassword"},
	// Redaction needs the raw value to build every rendering it scrubs.
	{file: "pkg/postgres/tracelog_redact.go", function: "newTraceRedactor"},
	// The Kubernetes Secret write for claim credentials needs plaintext for
	// the data keys and the credential stamp.
	{file: "pkg/controller/claimsecret.go", function: "ApplyClaimCredentials"},
	// Stamp verification hashes the stored Secret content.
	{file: "pkg/controller/claimsecret.go", function: "CredentialStampMatches"},
	// The Kubernetes Secret write for the internal admin instance.
	{file: "pkg/postgres/instance/connection.go", function: "AdminSecret"},
}

// TestRevealCallSitesMatchAllowlist makes the Reveal boundary a CI gate: the
// unit-test target runs this package, so a new plaintext access fails the build
// instead of waiting for review.
func TestRevealCallSitesMatchAllowlist(t *testing.T) {
	g := NewWithT(t)

	sources, err := repositorySources()
	g.Expect(err).NotTo(HaveOccurred())

	found, err := scanRevealCalls(sources)
	g.Expect(err).NotTo(HaveOccurred())

	if diff := revealAllowlistDiff(found, allowedRevealSites); diff != "" {
		t.Fatal(diff)
	}
}

// TestRepositorySourcesExcludeTestFiles keeps the inventory limited to shipped
// sources; redaction tests reveal sentinels by design.
func TestRepositorySourcesExcludeTestFiles(t *testing.T) {
	g := NewWithT(t)

	sources, err := repositorySources()
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(sources).To(HaveKey("pkg/postgres/ddl.go"))
	for file := range sources {
		g.Expect(file).NotTo(HaveSuffix("_test.go"), file)
	}
}

func TestScanRevealCalls(t *testing.T) {
	cases := []struct {
		name    string
		sources map[string]string
		want    []revealSite
	}{
		{
			name: "reports-call-in-function",
			sources: map[string]string{
				exampleSource: `package example

func deliver(value credential) string {
	return downstream(value.Reveal())
}
`,
			},
			want: []revealSite{{file: exampleSource, function: "deliver"}},
		},
		{
			name: "attributes-closure-to-enclosing-function",
			sources: map[string]string{
				exampleSource: `package example

func deliver(values []credential) {
	consume(func() string {
		return values[0].Reveal()
	})
}
`,
			},
			want: []revealSite{{file: exampleSource, function: "deliver"}},
		},
		{
			name: "reports-escaped-method-value",
			sources: map[string]string{
				exampleSource: `package example

func capture(value credential) {
	accessor := value.Reveal
	consume(accessor)
}
`,
			},
			want: []revealSite{{file: exampleSource, function: "capture"}},
		},
		{
			name: "reports-package-scope-use",
			sources: map[string]string{
				exampleSource: `package example

var plaintext = credential("sentinel").Reveal()
`,
			},
			want: []revealSite{{file: exampleSource}},
		},
		{
			name: "ignores-other-selectors",
			sources: map[string]string{
				exampleSource: `package example

func inspect(value credential) bool {
	return !value.IsZero() && value.Redact() != "" && value.RevealPassword() == ""
}
`,
			},
			want: []revealSite{},
		},
		{
			name: "ignores-method-declaration",
			sources: map[string]string{
				exampleSource: `package example

func (c credential) Reveal() string {
	return c.plaintext()
}
`,
			},
			want: []revealSite{},
		},
		{
			name: "ignores-generated-source",
			sources: map[string]string{
				"pkg/example/zz_generated.deepcopy.go": `// Code generated by controller-gen. DO NOT EDIT.

package example

func generated(value credential) string {
	return value.Reveal()
}
`,
			},
			want: []revealSite{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			sites, err := scanRevealCalls(tc.sources)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(sites).To(Equal(tc.want))
		})
	}
}

func TestScanRevealCallsRejectsUnparsableSource(t *testing.T) {
	g := NewWithT(t)

	_, err := scanRevealCalls(map[string]string{"pkg/broken/broken.go": "package broken\n\nfunc ("})

	g.Expect(err).To(MatchError(ContainSubstring("pkg/broken/broken.go")))
}

func TestRevealAllowlistDiff(t *testing.T) {
	cases := []struct {
		name         string
		found        []revealSite
		allowed      []revealSite
		wantEmpty    bool
		wantContains []string
	}{
		{
			name:      "in-sync",
			found:     []revealSite{sinkSite},
			allowed:   []revealSite{sinkSite},
			wantEmpty: true,
		},
		{
			name: "new-site",
			found: []revealSite{
				sinkSite,
				{file: "pkg/example/new.go", function: "deliver"},
			},
			allowed:      []revealSite{sinkSite},
			wantContains: []string{"+ pkg/example/new.go:deliver", "allowedRevealSites", "security review"},
		},
		{
			name:  "removed-site",
			found: []revealSite{sinkSite},
			allowed: []revealSite{
				sinkSite,
				{file: "pkg/example/old.go", function: "retired"},
			},
			wantContains: []string{"- pkg/example/old.go:retired", "security review"},
		},
		{
			name:         "package-scope-site",
			found:        []revealSite{{file: exampleSource}},
			allowed:      []revealSite{},
			wantContains: []string{"+ pkg/example/example.go:" + packageScopeSite},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			diff := revealAllowlistDiff(tc.found, tc.allowed)

			if tc.wantEmpty {
				g.Expect(diff).To(BeEmpty())
				return
			}
			for _, substring := range tc.wantContains {
				g.Expect(diff).To(ContainSubstring(substring))
			}
		})
	}
}

// scanRevealCalls parses sources as one inventory. The result is sorted and
// deduplicated so callers compare sites rather than occurrences.
func scanRevealCalls(sources map[string]string) ([]revealSite, error) {
	sites := make([]revealSite, 0, len(sources))
	for file, content := range sources {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, content, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", file, err)
		}
		if ast.IsGenerated(parsed) {
			continue
		}
		sites = append(sites, revealSitesIn(parsed, file)...)
	}
	slices.SortFunc(sites, compareRevealSites)
	return slices.Compact(sites), nil
}

// revealSitesIn reports the reveal uses in one parsed file. Package-level
// declarations have no enclosing function, so they carry an empty function and
// still require an explicit allowlist entry.
func revealSitesIn(file *ast.File, name string) []revealSite {
	sites := make([]revealSite, 0, 1)
	for _, decl := range file.Decls {
		function := ""
		if declaration, ok := decl.(*ast.FuncDecl); ok {
			function = declaration.Name.Name
		}
		ast.Walk(&revealUseVisitor{sites: &sites, site: revealSite{file: name, function: function}}, decl)
	}
	return sites
}

// revealUseVisitor matches the selector rather than only the call so an escaped
// method value (accessor := value.Reveal) cannot leave the inventory.
type revealUseVisitor struct {
	site  revealSite
	sites *[]revealSite
}

func (v *revealUseVisitor) Visit(node ast.Node) ast.Visitor {
	if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == revealMethod {
		*v.sites = append(*v.sites, v.site)
	}
	return v
}

// revealAllowlistDiff renders the inventory against the allowlist. New and
// removed sites both fail: an entry for a site that no longer reveals hides a
// future access behind a stale approval.
func revealAllowlistDiff(found, allowed []revealSite) string {
	added := siteDifference(found, allowed)
	removed := siteDifference(allowed, found)
	if len(added) == 0 && len(removed) == 0 {
		return ""
	}

	report := &strings.Builder{}
	report.WriteString("Reveal() sites do not match allowedRevealSites in pkg/secret/allowlist_test.go:\n")
	for _, site := range added {
		fmt.Fprintf(report, "\n  + %s (new plaintext access)\n", site)
	}
	for _, site := range removed {
		fmt.Fprintf(report, "\n  - %s (allowlisted site no longer reveals)\n", site)
	}
	report.WriteString(
		"\nAdd a site only after a security review confirms the sink needs plaintext, " +
			"and remove stale entries so the allowlist stays the exact inventory.\n",
	)
	return report.String()
}

func siteDifference(left, right []revealSite) []revealSite {
	known := make(map[revealSite]struct{}, len(right))
	for _, site := range right {
		known[site] = struct{}{}
	}
	difference := make([]revealSite, 0, len(left))
	for _, site := range left {
		if _, ok := known[site]; ok {
			continue
		}
		difference = append(difference, site)
	}
	slices.SortFunc(difference, compareRevealSites)
	return difference
}

func compareRevealSites(a, b revealSite) int {
	if order := strings.Compare(a.file, b.file); order != 0 {
		return order
	}
	return strings.Compare(a.function, b.function)
}

// repositorySources reads every source file the inventory covers: repository Go
// files except tests, with generated files removed by scanRevealCalls.
func repositorySources() (map[string]string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return nil, fmt.Errorf("locating repository sources: runtime caller unavailable")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "../.."))

	sources := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if skippedSourceDir(entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(relative, ".go") || strings.HasSuffix(relative, "_test.go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sources[relative] = string(content)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading repository sources: %w", err)
	}
	return sources, nil
}

// skippedSourceDir lists directories that never hold hand-maintained operator
// source: version control, build output, and vendored dependencies.
func skippedSourceDir(name string) bool {
	switch name {
	case ".git", "bin", "vendor":
		return true
	default:
		return false
	}
}
