package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func validateJSON(t *testing.T, stdin string, args ...string) (validateView, result) {
	t.Helper()
	r := cli(t, fakeReader(), stdin, append([]string{"validate", "-o", "json"}, args...)...)
	var v validateView
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatalf("%v: %v\nstdout: %s\nstderr: %s", args, err, r.stdout, r.stderr)
	}
	return v, r
}

func TestValidateGood(t *testing.T) {
	v, r := validateJSON(t, "", "-f", "../validate/testdata/set/good.yaml")
	if r.code != 0 || v.Objects != 7 || v.Skipped != 1 || len(v.Problems) != 0 {
		t.Errorf("exit %d, got %+v", r.code, v)
	}
	r = cli(t, fakeReader(), "", "validate", "-f", "../validate/testdata/set/good.yaml").ok(t)
	diff(t, r.stdout, "Objects checked: 7. Documents of other API groups skipped: 1. Errors: 0. Warnings: 0.\n")
}

func TestValidateTable(t *testing.T) {
	r := cli(t, fakeReader(), "", "validate", "-f", "../validate/testdata/set/listen-port.yaml")
	if r.code != 1 {
		t.Errorf("exit %d", r.code)
	}
	diff(t, r.stdout, `SEVERITY  FILE                                       DOC  OBJECT                  FIELD                      MESSAGE
Error     ../validate/testdata/set/listen-port.yaml  4    SiteLink/b-wireguard-2  spec.wireguard.listenPort  Duplicate value: 31820: SiteLink/b-wireguard-1 uses this listen port

Objects checked: 4. Documents of other API groups skipped: 0. Errors: 1. Warnings: 0.
`)
}

// Warnings alone exit with 0.
func TestValidateWarnings(t *testing.T) {
	v, r := validateJSON(t, "", "-f", "../validate/testdata/set/peer-missing.yaml")
	if r.code != 0 || v.Errors != 0 || v.Warnings != 1 || v.Problems[0].Field != "spec.peer" {
		t.Errorf("exit %d, got %+v", r.code, v)
	}
}

// The CLI reports exactly what the fixtures of internal/validate expect.
func TestValidateFixtures(t *testing.T) {
	sets, _ := filepath.Glob("../validate/testdata/set/*.yaml")
	if len(sets) == 0 {
		t.Fatal("no fixtures")
	}
	for _, path := range sets {
		v, _ := validateJSON(t, "", "-f", path)
		var got []string
		for _, p := range v.Problems {
			got = append(got, fmt.Sprintf("%s %s: %s: %s", p.Severity, p.Object, p.Field, p.Message))
		}
		want := wants(t, path)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: got\n%s\nwant\n%s", path, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}

	// Each bad document's errors are among the CLI's problems for that document.
	bad, _ := filepath.Glob("../validate/testdata/bad/*.yaml")
	for _, path := range bad {
		v, r := validateJSON(t, "", "-f", path)
		if r.code != 1 {
			t.Errorf("%s: exit %d", path, r.code)
		}
		got := map[string]bool{}
		for _, p := range v.Problems {
			got[fmt.Sprintf("%d %s: %s", p.Document, p.Field, p.Message)] = true
		}
		data, _ := os.ReadFile(path)
		for i, d := range strings.Split(string(data), "\n---\n") {
			for _, line := range strings.Split(d, "\n") {
				if w, ok := strings.CutPrefix(line, "# want: "); ok && !got[fmt.Sprintf("%d %s", i+1, w)] {
					t.Errorf("%s document %d: missing %q", path, i+1, w)
				}
			}
		}
	}
}

func wants(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if w, ok := strings.CutPrefix(line, "# want: "); ok {
			out = append(out, w)
		}
	}
	return out
}

func TestValidateSources(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("site.yaml", "apiVersion: strait.tako.id/v1alpha1\nkind: Site\nmetadata:\n  name: a\n")
	write("nested/peer.yml", "# comment only\n---\napiVersion: strait.tako.id/v1alpha1\nkind: SitePeer\nmetadata:\n  name: b\n")
	write("nested/deeper/link.json", `{"apiVersion": "strait.tako.id/v1alpha1", "kind": "SiteLink", "metadata": {"name": "b-direct"},
		"spec": {"peer": "b", "transport": "direct", "encryption": "None", "direct": {"address": "192.0.2.10"}}}`)
	write("notes.txt", "not a manifest")

	v, r := validateJSON(t, "", "-f", dir)
	if r.code != 0 || v.Objects != 3 || len(v.Problems) != 0 {
		t.Errorf("directory: exit %d, got %+v", r.code, v)
	}

	stdin := "apiVersion: strait.tako.id/v1alpha1\nkind: SiteLink\nmetadata:\n  name: x\nspec:\n  peer: b\n  transport: direct\n  direct:\n    address: 192.0.2.10\n"
	v, r = validateJSON(t, stdin, "-f", "-", "-f", filepath.Join(dir, "nested", "peer.yml"))
	if r.code != 1 || v.Objects != 2 || v.Errors != 1 || v.Problems[0].File != "-" || v.Problems[0].Document != 1 {
		t.Errorf("stdin: exit %d, got %+v", r.code, v)
	}

	r = cli(t, fakeReader(), "", "validate", "-f", filepath.Join(dir, "missing.yaml"))
	if r.code != 1 || !strings.Contains(r.stderr, "missing.yaml") {
		t.Errorf("missing file: exit %d, stderr %q", r.code, r.stderr)
	}
}

// A document that cannot be decoded is an error that names its file, document and object.
func TestValidateDecodeErrors(t *testing.T) {
	stdin := `apiVersion: strait.tako.id/v1alpha1
kind: SiteService
metadata:
  name: pg
  namespace: app
spec:
  mode: Single
  unknownhold: 30s
---
apiVersion: strait.tako.id/v1alpha1
kind: Site
metadata:
  name: a
spec:
  exchange:
    pollInterval: often
---
apiVersion: strait.tako.id/v1alpha1
kind: Route
metadata:
  name: r
---
apiVersion: strait.tako.id/v2
kind: Site
metadata:
  name: b
---
metadata:
  name: no-kind
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: other
`
	v, r := validateJSON(t, stdin, "-f", "-")
	// The SiteService with an unknown field is still checked, and has four errors of its own.
	if r.code != 1 || v.Objects != 1 || v.Skipped != 1 || v.Errors != 9 {
		t.Fatalf("exit %d, got %+v", r.code, v)
	}
	for i, want := range []struct{ object, message string }{
		{"SiteService/app/pg", `unknown field "spec.unknownhold"`},
		{"Site/a", `time: invalid duration "often"`},
		{"Route/r", "Route is not a kind of strait.tako.id/v1alpha1"},
		{"Site/b", "Site is not a kind of strait.tako.id/v1alpha1"},
		{"/no-kind", "apiVersion and kind are required"},
	} {
		found := false
		for _, p := range v.Problems {
			found = found || (p.Document == i+1 && p.Object.String() == want.object && p.Message == want.message)
		}
		if !found {
			t.Errorf("document %d: no problem %s %q in %+v", i+1, want.object, want.message, v.Problems)
		}
	}
	for i := 1; i < len(v.Problems); i++ {
		if v.Problems[i-1].Document > v.Problems[i].Document {
			t.Errorf("problems are not in document order")
		}
	}
}
