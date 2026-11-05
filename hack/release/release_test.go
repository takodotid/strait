// Package release tests the scripts of the release workflow (RFC0007, requirements 17, 20 and 21) against fakes of gh
// and curl, which answer from fixtures.
package release

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
// manifest request with the HTTP status in $FAKE_MANIFEST, or with -D, with headers that hold that status and the
// digest in $FAKE_DIGEST.
const fakeCurl = `#!/usr/bin/env bash
headers=0
for a; do
	[[ $a == https://* ]] && url=$a
	[[ $a == -D ]] && headers=1
done
case $url in
*/token\?*) [[ ${FAKE_TOKEN:-} == fail ]] && { echo "curl: (22) 403" >&2; exit 22; }; echo '{"token":"t"}' ;;
*/manifests/*)
	if [[ $headers == 1 ]]; then
		printf 'HTTP/2 %s\r\n' "$FAKE_MANIFEST"
		[[ -n ${FAKE_DIGEST:-} ]] && printf 'docker-content-digest: %s\r\n' "$FAKE_DIGEST"
		printf '\r\n'
	else
		printf %s "$FAKE_MANIFEST"
	fi
	;;
*) echo "fake curl: $url" >&2; exit 2 ;;
esac
`

// fakeDocker records the arguments of docker buildx imagetools create in $FAKE_GH/create, and answers inspect with
// the digest in $FAKE_DIGEST.
const fakeDocker = `#!/usr/bin/env bash
[[ $1 == buildx && $2 == imagetools ]] || { echo "fake docker: $*" >&2; exit 2; }
case $3 in
create) shift 3; printf '%s\n' "$@" >"$FAKE_GH/create" ;;
inspect) printf '{"digest":"%s"}' "$FAKE_DIGEST" ;;
*) echo "fake docker: $*" >&2; exit 2 ;;
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
	for name, body := range map[string]string{"gh": fakeGH, "curl": fakeCurl, "docker": fakeDocker} {
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
	if b, err := os.ReadFile(filepath.Join(data, "create")); err == nil {
		out = append(out, "\ncreate: "+strings.Join(strings.Fields(string(b)), " ")...)
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
	// gate.sh reads the jobs from ci.yaml: each one counts.
	for _, job := range jobs {
		t.Run("without "+job, func(t *testing.T) {
			fixtures := base()
			fixtures[jobs1] = jobsJSON(slices.DeleteFunc(slices.Clone(jobs), func(j string) bool { return j == job }), nil)
			out, ok := run(t, fixtures, nil, "gate.sh", repo, tag, commit)
			if ok || !strings.Contains(out, "not successful: "+job) {
				t.Errorf("exit 0: %v, want a refusal that names %s: %s", ok, job, out)
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

// pinned.sh refuses an action that is not pinned by commit SHA (RFC0007, requirement 24), and passes the repository.
func TestPinned(t *testing.T) {
	sha := strings.Repeat("0", 40)
	for _, tt := range []struct {
		uses string
		ok   bool
	}{
		{"actions/checkout@" + sha + " # v7.0.1", true},
		{"actions/checkout@" + sha, true},
		{"./.github/actions/setup-helm", true},
		{"actions/checkout@v7", false},
		{"actions/checkout@main", false},
		{"actions/checkout@" + sha[:39], false},
		{"docker://alpine:3", false},
	} {
		root := t.TempDir()
		dir := filepath.Join(root, ".github", "workflows")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		workflow := "jobs:\n  a:\n    steps:\n      - uses: " + tt.uses + "\n"
		if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(workflow), 0o644); err != nil {
			t.Fatal(err)
		}
		out, ok := run(t, nil, nil, "pinned.sh", root)
		if ok != tt.ok {
			t.Errorf("uses: %s: exit 0: %v, want %v: %s", tt.uses, ok, tt.ok, out)
		}
	}
	if out, ok := run(t, nil, nil, "pinned.sh"); !ok {
		t.Errorf("the repository: %s", out)
	}
}

// tags.sh points the tags at the digest, and fails unless each tag then names it (RFC0007, requirement 22).
func TestTags(t *testing.T) {
	digest := "sha256:" + strings.Repeat("e", 64)
	source := "ghcr.io/o/strait@" + digest
	for _, tt := range []struct {
		name string
		args []string
		got  string
		ok   bool
		want string
	}{
		{"two tags", []string{source, "ghcr.io/o/strait:1.3.0", "ghcr.io/o/strait:1.3"}, digest, true,
			"create: --tag ghcr.io/o/strait:1.3.0 --tag ghcr.io/o/strait:1.3 " + source},
		{"an empty tag", []string{source, "", "ghcr.io/o/strait:1.3.0"}, digest, true, "create: --tag ghcr.io/o/strait:1.3.0 " + source},
		{"a tag names another digest", []string{source, "ghcr.io/o/strait:1.3.0"}, "sha256:" + strings.Repeat("f", 64), false, "names sha256:fff"},
		{"no tag", []string{source, ""}, digest, false, "no tag given"},
		{"no digest", []string{"ghcr.io/o/strait:1.3.0", "ghcr.io/o/strait:1.3"}, digest, false, "is not IMAGE@sha256:"},
		{"empty digest", []string{"ghcr.io/o/strait@", "ghcr.io/o/strait:1.3"}, digest, false, "is not IMAGE@sha256:"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, ok := run(t, nil, []string{"FAKE_DIGEST=" + tt.got}, "tags.sh", tt.args...)
			if ok != tt.ok {
				t.Fatalf("exit 0: %v, want %v: %s", ok, tt.ok, out)
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("output lacks %q: %s", tt.want, out)
			}
		})
	}
}

// needs is a job's needs, one job or a list.
type needs []string

func (n *needs) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*n = needs{one}
		return nil
	}
	return json.Unmarshal(b, (*[]string)(n))
}

// releaseJob is what TestReleaseWorkflow reads of a job of release.yaml.
type releaseJob struct {
	Needs       needs
	Permissions map[string]string
	Steps       []releaseStep
}

// releaseStep is a step of a job of release.yaml.
type releaseStep struct {
	Uses, Run string
	With      map[string]string
}

// step returns the index of the first step whose script holds all the strings given, or -1.
func (j releaseJob) step(holds ...string) int {
	for i, s := range j.Steps {
		ok := true
		for _, h := range holds {
			ok = ok && strings.Contains(s.Run, h)
		}
		if ok {
			return i
		}
	}
	return -1
}

// before reports whether the script holds a before b.
func before(script, a, b string) bool {
	i, j := strings.Index(script, a), strings.Index(script, b)
	return i >= 0 && j > i
}

// runsGo matches a step that runs Go on the runner. make counts, since the Makefile runs go env as it loads.
var runsGo = regexp.MustCompile(`(^|[^a-z_./-])(make|go)( |$)`)

// The release runs every test before any push, and runs Go only in jobs that can write nothing (RFC0007, requirements
// 17, 18 and 19).
func TestReleaseWorkflow(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Permissions map[string]string
		Concurrency struct {
			CancelInProgress bool `json:"cancel-in-progress"`
		}
		Jobs map[string]releaseJob
	}
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	writes := func(p map[string]string) []string {
		var got []string
		for k, v := range p {
			if v != "read" && v != "none" {
				got = append(got, k+": "+v)
			}
		}
		slices.Sort(got)
		return got
	}
	if got := writes(w.Permissions); len(got) > 0 {
		t.Errorf("the workflow's permissions grant %v to every job", got)
	}
	if w.Concurrency.CancelInProgress {
		t.Error("a new run cancels a release in progress")
	}
	// after reports whether job a needs job b, directly or not.
	var after func(a, b string, seen map[string]bool) bool
	after = func(a, b string, seen map[string]bool) bool {
		if seen[a] {
			return false
		}
		seen[a] = true
		for _, n := range w.Jobs[a].Needs {
			if n == b || after(n, b, seen) {
				return true
			}
		}
		return false
	}
	gate, ok := w.Jobs["gate"]
	if !ok || len(gate.Needs) > 0 {
		t.Fatal("no first job gate")
	}
	test, ok := w.Jobs["test"]
	if !ok {
		t.Fatal("no job test")
	}
	var script strings.Builder
	for _, s := range test.Steps {
		script.WriteString(s.Run + "\n")
	}
	for _, want := range []string{"make verify test", "make dist"} {
		if !strings.Contains(script.String(), want) {
			t.Errorf("job test does not run %s", want)
		}
	}
	for name, job := range w.Jobs {
		if name != "gate" && !after(name, "gate", map[string]bool{}) {
			t.Errorf("job %s does not need gate", name)
		}
		w := writes(job.Permissions)
		if len(w) == 0 {
			continue
		}
		if !after(name, "test", map[string]bool{}) {
			t.Errorf("job %s holds %v and does not need test", name, w)
		}
		for _, s := range job.Steps {
			if strings.HasPrefix(s.Uses, "actions/setup-go@") || runsGo.MatchString(s.Run) {
				t.Errorf("job %s holds %v and runs Go: %s%s", name, w, s.Uses, s.Run)
			}
		}
	}

	// Requirement 20: gate checks the three, and each push checks its own right before it.
	if gate.step("absent.sh oci", "absent.sh release") < 0 {
		t.Error("gate does not check that the version is absent")
	}
	publish := w.Jobs["publish"]
	chart := publish.step("helm push")
	release := publish.step("gh release create")
	tags := publish.step("tags.sh")
	if chart < 0 || release < 0 || tags < 0 {
		t.Fatalf("publish: the chart's push at step %d, the release at %d, the tags at %d", chart, release, tags)
	}
	for _, c := range []struct {
		i           int
		check, push string
	}{
		{chart, "absent.sh oci", "helm push"},
		{release, "absent.sh release", "gh release create"},
		{tags, "absent.sh oci", "tags.sh"},
	} {
		if !before(publish.Steps[c.i].Run, c.check, c.push) {
			t.Errorf("publish: %s is not checked right before it", c.push)
		}
	}
	// Requirement 22: the tags last, once the image and the chart are signed.
	if !(chart < release && release < tags) {
		t.Errorf("publish: the chart's push at step %d, the release at %d, the tags at %d, want this order", chart, release, tags)
	}
	if !before(publish.Steps[chart].Run, "helm push", "cosign sign") {
		t.Error("publish: the chart is not signed after its push")
	}
	if w.Jobs["image"].step("cosign sign") < 0 {
		t.Error("image: the image is not signed")
	}
	// Requirement 23: every asset of the GitHub release is attested before the first push.
	attest := -1
	for i, s := range publish.Steps {
		if strings.HasPrefix(s.Uses, "actions/attest@") {
			attest = i
		}
	}
	if attest < 0 || attest > chart {
		t.Fatalf("publish: attestations at step %d, the chart's push at %d", attest, chart)
	}
	for _, f := range []string{"dist/*.tar.gz", "dist/*.zip", "dist/*.tgz", "dist/strait-crds.yaml", "dist/SHA256SUMS"} {
		if !slices.Contains(strings.Fields(publish.Steps[attest].With["subject-path"]), f) {
			t.Errorf("publish: %s is not attested", f)
		}
	}
}

// digest.sh prints the digest that the registry gives for a tag, and fails on anything else.
func TestDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("9", 64)
	for _, tt := range []struct {
		name string
		args []string
		env  []string
		ok   bool
		want string
	}{
		{"pushed", []string{"ghcr.io/o/charts/strait:1.3.0"}, []string{"FAKE_MANIFEST=200", "FAKE_DIGEST=" + digest}, true, digest + "\n"},
		{"absent", []string{"ghcr.io/o/charts/strait:1.3.0"}, []string{"FAKE_MANIFEST=404", "FAKE_DIGEST=" + digest}, false, "HTTP 404"},
		{"no digest", []string{"ghcr.io/o/charts/strait:1.3.0"}, []string{"FAKE_MANIFEST=200", "FAKE_DIGEST="}, false, "no sha256 digest"},
		{"other algorithm", []string{"ghcr.io/o/charts/strait:1.3.0"}, []string{"FAKE_MANIFEST=200", "FAKE_DIGEST=sha512:ab"}, false, "no sha256 digest"},
		{"no token", []string{"ghcr.io/o/charts/strait:1.3.0"}, []string{"FAKE_TOKEN=fail", "FAKE_MANIFEST=200", "FAKE_DIGEST=" + digest}, false, "no pull token"},
		{"no tag", []string{"ghcr.io/o/charts/strait"}, []string{"FAKE_MANIFEST=200", "FAKE_DIGEST=" + digest}, false, "is not HOST/NAME:TAG"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, ok := run(t, nil, tt.env, "digest.sh", tt.args...)
			if ok != tt.ok {
				t.Fatalf("exit 0: %v, want %v: %s", ok, tt.ok, out)
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("output lacks %q: %s", tt.want, out)
			}
		})
	}
}
