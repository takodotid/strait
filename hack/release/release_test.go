// Package release tests the scripts of the release workflow (RFC0007, requirements 17, 20 and 21) against fakes of gh
// and curl, which answer from fixtures.
package release

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// fakeGH answers gh api PATH [--jq EXPR] from the file named after PATH in $FAKE_GH, through jq. A file PATH.error
// holds the error that gh prints. Without either, it answers as GitHub does for a missing object.
const fakeGH = `#!/usr/bin/env bash
set -euo pipefail
[[ $1 == api ]] || { echo "fake gh: $*" >&2; exit 2; }
shift
path= expr=.
while [[ $# -gt 0 ]]; do
	case $1 in
	--paginate) ;;
	--jq) expr=$2; shift ;;
	*) path=$1 ;;
	esac
	shift
done
f=$FAKE_GH/$(printf %s "$path" | tr -c 'A-Za-z0-9.' '_')
if [[ -f $f.error ]]; then cat "$f.error" >&2; exit 1; fi
if [[ ! -f $f ]]; then echo 'gh: Not Found (HTTP 404)' >&2; exit 1; fi
jq -r "$expr" "$f"
`

// fakeCurl answers a token request with a token, or fails as curl -f does when $FAKE_TOKEN is "fail". It answers a
// manifest request with the HTTP status in $FAKE_MANIFEST.
const fakeCurl = `#!/usr/bin/env bash
for a; do [[ $a == https://* ]] && url=$a; done
case $url in
*/token\?*) [[ ${FAKE_TOKEN:-} == fail ]] && { echo "curl: (22) 403" >&2; exit 22; }; echo '{"token":"t"}' ;;
*/manifests/*) printf %s "$FAKE_MANIFEST" ;;
*) echo "fake curl: $url" >&2; exit 2 ;;
esac
`

// fixtureName is the file name that fakeGH reads for an API path.
func fixtureName(path string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' {
			return r
		}
		return '_'
	}, path)
}

// run runs a script of this directory with the fakes first in PATH, the fixtures given, and the environment given. It
// returns the combined output and whether the script exited 0.
func run(t *testing.T, fixtures map[string]string, env []string, script string, args ...string) (string, bool) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("jq is not installed; CI must run this test")
		}
		t.Skip("jq is not installed")
	}
	bin, data := t.TempDir(), t.TempDir()
	for name, body := range map[string]string{"gh": fakeGH, "curl": fakeCurl} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range fixtures {
		name := fixtureName(strings.TrimSuffix(path, ".error"))
		if strings.HasSuffix(path, ".error") {
			name += ".error"
		}
		if err := os.WriteFile(filepath.Join(data, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("./"+script, args...)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_GH="+data)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		t.Fatal(err)
	}
	return string(out), err == nil
}

// ciJobs returns the keys of the jobs of ci.yaml.
func ciJobs(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var ci struct{ Jobs map[string]any }
	if err := yaml.Unmarshal(b, &ci); err != nil {
		t.Fatal(err)
	}
	if len(ci.Jobs) == 0 {
		t.Fatal("ci.yaml has no jobs")
	}
	return slices.Sorted(maps.Keys(ci.Jobs))
}

// jobsJSON is the jobs of a run, each with the conclusion given, or success.
func jobsJSON(jobs []string, conclusions map[string]string) string {
	var b strings.Builder
	b.WriteString(`{"jobs":[`)
	for i, j := range jobs {
		c := conclusions[j]
		if c == "" {
			c = "success"
		}
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"name":"` + j + `","conclusion":"` + c + `"}`)
	}
	b.WriteString("]}")
	return b.String()
}

func TestGate(t *testing.T) {
	const (
		repo   = "o/r"
		tag    = "v1.3.0"
		commit = "c0ffee"
		runs   = "repos/o/r/actions/workflows/ci.yaml/runs?head_sha=c0ffee&event=push&branch=main&per_page=100"
		jobs1  = "repos/o/r/actions/runs/1/jobs?filter=latest&per_page=100"
		jobs2  = "repos/o/r/actions/runs/2/jobs?filter=latest&per_page=100"
	)
	jobs := ciJobs(t)
	last := jobs[len(jobs)-1]
	base := func() map[string]string {
		return map[string]string{
			"repos/o/r/git/ref/tags/v1.3.0":   `{"object":{"type":"tag","sha":"7a9"}}`,
			"repos/o/r/git/tags/7a9":          `{"tag":"v1.3.0","object":{"sha":"c0ffee"},"verification":{"verified":true,"reason":"valid"}}`,
			"repos/o/r/compare/c0ffee...main": `{"status":"ahead"}`,
			runs:                              `{"workflow_runs":[{"id":1,"status":"completed","conclusion":"success"}]}`,
			jobs1:                             jobsJSON(jobs, nil),
		}
	}
	for _, tt := range []struct {
		name   string
		tag    string
		change map[string]string
		ok     bool
		want   string
	}{
		{name: "released", ok: true, want: "every job successful"},
		{name: "commit is main", change: map[string]string{"repos/o/r/compare/c0ffee...main": `{"status":"identical"}`}, ok: true},
		{name: "a failed run, then a successful one", ok: true, change: map[string]string{
			runs:  `{"workflow_runs":[{"id":2,"status":"completed","conclusion":"failure"},{"id":1,"status":"completed","conclusion":"success"}]}`,
			jobs2: jobsJSON(jobs, map[string]string{last: "failure"}),
		}},
		{name: "not vX.Y.Z", tag: "v1.3", want: "is not vX.Y.Z"},
		{name: "pre-release", tag: "v1.3.0-rc.1", want: "is not vX.Y.Z"},
		{name: "no such tag", tag: "v1.3.1", want: "cannot read tag v1.3.1"},
		{name: "lightweight tag", change: map[string]string{"repos/o/r/git/ref/tags/v1.3.0": `{"object":{"type":"commit","sha":"c0ffee"}}`}, want: "not annotated"},
		{name: "unsigned", change: map[string]string{"repos/o/r/git/tags/7a9": `{"tag":"v1.3.0","object":{"sha":"c0ffee"},"verification":{"verified":false,"reason":"unsigned"}}`}, want: "does not verify the signature of tag v1.3.0: unsigned"},
		{name: "unknown key", change: map[string]string{"repos/o/r/git/tags/7a9": `{"tag":"v1.3.0","object":{"sha":"c0ffee"},"verification":{"verified":false,"reason":"unknown_key"}}`}, want: "unknown_key"},
		{name: "another commit", change: map[string]string{"repos/o/r/git/tags/7a9": `{"tag":"v1.3.0","object":{"sha":"beef"},"verification":{"verified":true,"reason":"valid"}}`}, want: "points at beef, not at c0ffee"},
		{name: "not on main", change: map[string]string{"repos/o/r/compare/c0ffee...main": `{"status":"diverged"}`}, want: "is not on main: main is diverged"},
		{name: "ahead of main", change: map[string]string{"repos/o/r/compare/c0ffee...main": `{"status":"behind"}`}, want: "is not on main"},
		{name: "compare fails", change: map[string]string{"repos/o/r/compare/c0ffee...main.error": "gh: Server Error (HTTP 500)"}, want: "cannot compare"},
		{name: "ci running", change: map[string]string{runs: `{"workflow_runs":[{"id":1,"status":"in_progress","conclusion":null}]}`}, want: "has not finished"},
		{name: "ci failed", change: map[string]string{runs: `{"workflow_runs":[{"id":1,"status":"completed","conclusion":"failure"}]}`}, want: "no ci run"},
		{name: "no ci run", change: map[string]string{runs: `{"workflow_runs":[]}`}, want: "no ci run"},
		{name: "ci runs unreadable", change: map[string]string{runs + ".error": "gh: Bad credentials (HTTP 401)"}, want: "cannot list the ci runs"},
		{name: "a job skipped", change: map[string]string{jobs1: jobsJSON(jobs, map[string]string{last: "skipped"})}, want: "not successful: " + last},
		{name: "a job missing", change: map[string]string{jobs1: jobsJSON(jobs[:len(jobs)-1], nil)}, want: "not successful: " + last},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixtures := base()
			for k, v := range tt.change {
				if !strings.HasSuffix(k, ".error") {
					fixtures[k] = v
					continue
				}
				delete(fixtures, strings.TrimSuffix(k, ".error"))
				fixtures[k] = v
			}
			name := tt.tag
			if name == "" {
				name = tag
			}
			out, ok := run(t, fixtures, nil, "gate.sh", repo, name, commit)
			if ok != tt.ok {
				t.Fatalf("exit 0: %v, want %v: %s", ok, tt.ok, out)
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("output lacks %q: %s", tt.want, out)
			}
		})
	}
}

func TestAbsent(t *testing.T) {
	const release = "repos/o/r/releases/tags/v1.3.0"
	for _, tt := range []struct {
		name     string
		args     []string
		fixtures map[string]string
		env      []string
		ok       bool
		want     string
	}{
		{name: "image absent", args: []string{"oci", "ghcr.io/o/strait:1.3.0"}, env: []string{"FAKE_MANIFEST=404"}, ok: true, want: "absent"},
		{name: "image published", args: []string{"oci", "ghcr.io/o/strait:1.3.0"}, env: []string{"FAKE_MANIFEST=200"}, want: "published already"},
		{name: "registry fails", args: []string{"oci", "ghcr.io/o/strait:1.3.0"}, env: []string{"FAKE_MANIFEST=500"}, want: "HTTP 500, so its absence cannot be told"},
		{name: "registry refuses", args: []string{"oci", "ghcr.io/o/strait:1.3.0"}, env: []string{"FAKE_MANIFEST=401"}, want: "cannot be told"},
		{name: "no token", args: []string{"oci", "ghcr.io/o/charts/strait:1.3.0"}, env: []string{"FAKE_TOKEN=fail", "FAKE_MANIFEST=404"}, want: "no pull token"},
		{name: "no tag", args: []string{"oci", "ghcr.io/o/strait"}, env: []string{"FAKE_MANIFEST=404"}, want: "is not HOST/NAME:TAG"},
		{name: "release absent", args: []string{"release", "o/r", "v1.3.0"}, ok: true, want: "absent"},
		{name: "release published", args: []string{"release", "o/r", "v1.3.0"}, fixtures: map[string]string{release: `{"html_url":"https://example/v1.3.0"}`}, want: "published already"},
		{name: "GitHub fails", args: []string{"release", "o/r", "v1.3.0"}, fixtures: map[string]string{release + ".error": "gh: Server Error (HTTP 502)"}, want: "cannot be told"},
		{name: "GitHub refuses", args: []string{"release", "o/r", "v1.3.0"}, fixtures: map[string]string{release + ".error": "gh: Bad credentials (HTTP 401)"}, want: "cannot be told"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, ok := run(t, tt.fixtures, tt.env, "absent.sh", tt.args...)
			if ok != tt.ok {
				t.Fatalf("exit 0: %v, want %v: %s", ok, tt.ok, out)
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("output lacks %q: %s", tt.want, out)
			}
		})
	}
}

// chart.sh packages the chart with the digest, and checks the image of the package (RFC0007, requirement 21).
func TestChart(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is not installed; CI must run this test")
		}
		t.Skip("helm is not installed")
	}
	digest := "sha256:" + strings.Repeat("d", 64)
	for _, tt := range []struct {
		args []string
		ok   bool
		want string
	}{
		{[]string{"v1.3.0", digest}, true, "names ghcr.io/takodotid/strait:1.3.0@" + digest},
		{[]string{"v1.3.0"}, true, "names ghcr.io/takodotid/strait:1.3.0\n"},
		{[]string{"v1.3.0", "sha256:d"}, false, "is not sha256: and 64 hex digits"},
		// The release passes the image job's digest: an empty one is an error, not a chart without a digest.
		{[]string{"v1.3.0", ""}, false, "digest '' is not sha256: and 64 hex digits"},
	} {
		dist := t.TempDir()
		out, ok := run(t, nil, []string{"DIST=" + dist}, "chart.sh", tt.args...)
		if ok != tt.ok {
			t.Fatalf("%v: exit 0: %v, want %v: %s", tt.args, ok, tt.ok, out)
		}
		if !strings.Contains(out, tt.want) {
			t.Errorf("%v: output lacks %q: %s", tt.args, tt.want, out)
		}
		if _, err := os.Stat(filepath.Join(dist, "strait-1.3.0.tgz")); tt.ok && err != nil {
			t.Errorf("%v: %v", tt.args, err)
		}
	}
}
