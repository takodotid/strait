package release

import (
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// builtinPreset matches a preset that Renovate ships: a group of its own, or none for its default group, then a
// name, then optional arguments. The groups are those of Renovate's lib/config/presets/internal/index.ts. A preset of
// another repository (local>, github>, gitlab>, ...) or of a package (foo:bar, @scope/foo) does not match.
var builtinPreset = regexp.MustCompile(`^(abandonments|config|customManagers|default|docker|global|group|helpers|mergeConfidence|monorepo|packages|preview|replacements|schedule|security|workarounds)?:[A-Za-z0-9][A-Za-z0-9._-]*(\(.*\))?$`)

// presets returns every entry of every "extends" in v, at any depth, as "path: preset".
func presets(t *testing.T, v any, path string) []string {
	t.Helper()
	var out []string
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			if k != "extends" {
				out = append(out, presets(t, child, path+"."+k)...)
				continue
			}
			list, ok := child.([]any)
			if !ok {
				t.Errorf("%s.extends: %T, want a list", path, child)
				continue
			}
			for _, p := range list {
				s, ok := p.(string)
				if !ok {
					t.Errorf("%s.extends: %T, want a string", path, p)
					continue
				}
				out = append(out, s)
			}
		}
	case []any:
		for _, child := range v {
			out = append(out, presets(t, child, path+"[]")...)
		}
	}
	return out
}

// TestRenovatePresets checks that renovate.json extends Renovate's built-in presets only (RFC0007, requirement 27),
// so that no change outside this repository changes which updates Renovate proposes, or how.
func TestRenovatePresets(t *testing.T) {
	for _, tc := range []struct {
		preset string
		ok     bool
	}{
		{"config:recommended", true},
		{":semanticCommits", true},
		{"group:allNonMajor", true},
		{"helpers:pinGitHubActionDigests", true},
		{":timezone(Etc/UTC)", true},
		{"local>org/renovate-config", false},
		{"github>org/renovate-config", false},
		{"github>org/renovate-config#v1.0.0", false},
		{"gitlab>org/renovate-config", false},
		{"org/renovate-config", false},
		{"renovate-config-org", false},
		{"@org/renovate-config", false},
		{"org:base", false},
	} {
		if got := builtinPreset.MatchString(tc.preset); got != tc.ok {
			t.Errorf("%q: built-in %v, want %v", tc.preset, got, tc.ok)
		}
	}

	b, err := os.ReadFile("../../renovate.json")
	if err != nil {
		t.Fatal(err)
	}
	var config any
	if err := json.Unmarshal(b, &config); err != nil {
		t.Fatal(err)
	}
	got := presets(t, config, "")
	if len(got) == 0 {
		t.Fatal("renovate.json extends no preset; the test reads nothing")
	}
	for _, p := range got {
		if !builtinPreset.MatchString(p) {
			t.Errorf("renovate.json extends %q, which is not one of Renovate's built-in presets", p)
		}
	}
}

// TestRenovateCustomManagers checks that Renovate's custom managers find every pin that RFC0007 requirement 24 makes:
// the version, and the SHA-256 where Renovate can read it, of each tool of .github/actions/, and the release's BuildKit
// image by tag and digest (requirement 29). Go's regexp reads Renovate's named groups.
func TestRenovateCustomManagers(t *testing.T) {
	data, err := os.ReadFile("../../renovate.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		CustomManagers []struct {
			CustomType          string
			ManagerFilePatterns []string
			MatchStrings        []string
			DepNameTemplate     string
		}
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	// What each pinned file holds, and the dependency that must be found there.
	want := map[string]struct{ dep, value, digest string }{
		".github/actions/setup-cosign/action.yaml": {"sigstore/cosign", "VERSION: ", "SHA256: "},
		".github/actions/setup-helm/action.yaml":   {"helm/helm", "VERSION: ", ""},
		".github/actions/setup-bun/action.yaml":    {"oven-sh/bun", "VERSION: ", ""},
		".github/workflows/release.yaml":           {"moby/buildkit", "image=moby/buildkit:", "@"},
	}
	actions, err := filepath.Glob("../../.github/actions/*/action.yaml")
	if err != nil || len(actions) == 0 {
		t.Fatal("no actions in .github/actions")
	}
	for _, a := range actions {
		body, err := os.ReadFile(a)
		if err != nil {
			t.Fatal(err)
		}
		if name := strings.TrimPrefix(a, "../../"); strings.Contains(string(body), "VERSION: ") {
			if _, ok := want[name]; !ok {
				t.Errorf("%s pins a version that no custom manager of renovate.json updates", name)
			}
		}
	}
	found := map[string]bool{}
	for _, m := range cfg.CustomManagers {
		if m.CustomType != "regex" || len(m.ManagerFilePatterns) != 1 || len(m.MatchStrings) != 1 {
			t.Errorf("custom manager %+v: want a regex manager with one file pattern and one match string", m)
			continue
		}
		file := regexp.MustCompile(strings.Trim(m.ManagerFilePatterns[0], "/"))
		match := regexp.MustCompile(m.MatchStrings[0])
		for name, w := range want {
			if !file.MatchString(name) {
				continue
			}
			body, err := os.ReadFile("../../" + name)
			if err != nil {
				t.Fatal(err)
			}
			sub := match.FindStringSubmatch(string(body))
			if sub == nil {
				t.Errorf("%s: %q matches nothing", name, m.MatchStrings[0])
				continue
			}
			group := func(g string) string {
				if i := match.SubexpIndex(g); i >= 0 {
					return sub[i]
				}
				return ""
			}
			dep := cmp.Or(group("depName"), m.DepNameTemplate)
			value, digest := group("currentValue"), group("currentDigest")
			switch {
			case dep != w.dep:
				t.Errorf("%s: dependency %q, want %q", name, dep, w.dep)
			case value == "" || !strings.Contains(string(body), w.value+value):
				t.Errorf("%s: version %q is not the file's", name, value)
			case (w.digest == "") != (digest == ""):
				t.Errorf("%s: digest %q, want one only where Renovate can read it", name, digest)
			case digest != "" && !strings.Contains(string(body), w.digest+digest):
				t.Errorf("%s: digest %q is not the file's", name, digest)
			}
			found[name] = true
		}
	}
	for name := range want {
		if !found[name] {
			t.Errorf("no custom manager of renovate.json covers %s", name)
		}
	}
}
