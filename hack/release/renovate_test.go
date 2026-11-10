package release

import (
	"encoding/json"
	"os"
	"regexp"
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
