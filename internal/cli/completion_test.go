package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Each shell gets its script, which calls the binary's __complete. Another shell is a usage error.
func TestCompletionScripts(t *testing.T) {
	for shell, want := range map[string]string{
		"bash":       "__start_strait",
		"zsh":        "#compdef strait",
		"fish":       "complete -c strait",
		"powershell": "Register-ArgumentCompleter",
	} {
		r := cli(t, fakeReader(), "", "completion", shell).ok(t)
		if !strings.Contains(r.stdout, want) || !strings.Contains(r.stdout, "__complete") {
			t.Errorf("%s: no %q or __complete in the script", shell, want)
		}
	}
	if r := cli(t, fakeReader(), "", "completion", "tcsh"); r.code != 2 {
		t.Errorf("tcsh: exit %d, want 2: %s", r.code, r.stderr)
	}
}

// complete returns the completions of the words, and the directive line.
func complete(t *testing.T, r client.Reader, words ...string) ([]string, string) {
	t.Helper()
	res := cli(t, r, "", append([]string{"__complete"}, words...)...)
	if res.code != 0 {
		t.Fatalf("__complete %v: exit %d: %s", words, res.code, res.stderr)
	}
	var out []string
	directive := ""
	for _, line := range strings.Split(strings.TrimSpace(res.stdout), "\n") {
		if strings.HasPrefix(line, ":") {
			directive = line
			continue
		}
		name, _, _ := strings.Cut(line, "\t")
		out = append(out, name)
	}
	return out, directive
}

// Commands, the values of -o, and the names of SiteServices and their namespaces are completed. A command with no
// argument offers no file name.
func TestCompletions(t *testing.T) {
	r := fakeReader(cluster()...)
	for _, tt := range []struct {
		words []string
		want  string
	}{
		{[]string{""}, "completion explain help links peers status validate version"},
		{[]string{"ex"}, "explain"},
		{[]string{"explain", "-o", ""}, "table json yaml"},
		{[]string{"explain", ""}, "cache fresh pg"},
		{[]string{"explain", "p"}, "pg"},
		{[]string{"explain", "pg", ""}, ""},
		{[]string{"explain", "-A", ""}, "cache fresh pg"},
		{[]string{"status", "-n", ""}, "app other"},
		{[]string{"status", ""}, ""},
		{[]string{"completion", ""}, "bash zsh fish powershell"},
	} {
		got, directive := complete(t, r, tt.words...)
		if strings.Join(got, " ") != tt.want {
			t.Errorf("%v: %q, want %q", tt.words, strings.Join(got, " "), tt.want)
		}
		if tt.words[0] != "" && tt.words[0] != "ex" && directive != ":4" {
			t.Errorf("%v: directive %s, want :4, no file names", tt.words, directive)
		}
	}
}

// slowReader answers no list before its context ends.
type slowReader struct{ client.Reader }

func (slowReader) List(ctx context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	<-ctx.Done()
	return ctx.Err()
}

// A cluster that does not answer gives no names, within the time limit.
func TestCompletionTimeLimit(t *testing.T) {
	start := time.Now()
	got, directive := complete(t, slowReader{fakeReader()}, "explain", "")
	if took := time.Since(start); took > completionTimeout+time.Second {
		t.Errorf("the completion took %s", took)
	}
	if len(got) != 0 || directive != ":4" {
		t.Errorf("%v %s, want nothing", got, directive)
	}
}

// The kubeconfig's contexts are completed from the file. While completing, a credential plugin may not ask a person,
// and each request has the time limit; other commands keep the kubeconfig's settings.
func TestCompletionKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(`apiVersion: v1
kind: Config
current-context: site-a
clusters:
- {name: a, cluster: {server: "https://192.0.2.1:6443"}}
contexts:
- {name: site-a, context: {cluster: a, user: a}}
- {name: site-b, context: {cluster: a, user: a}}
users:
- name: a
  user:
    exec: {apiVersion: client.authentication.k8s.io/v1, command: login-tool, interactiveMode: Always}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ := complete(t, fakeReader(), "status", "--kubeconfig", path, "--context", "site-")
	if strings.Join(got, " ") != "site-a site-b" {
		t.Errorf("contexts: %v", got)
	}
	for _, completing := range []bool{false, true} {
		cfg, _, err := restConfig(&options{kubeconfig: path, completing: completing})
		if err != nil {
			t.Fatal(err)
		}
		mode, timeout := cfg.ExecProvider.InteractiveMode, cfg.Timeout
		if completing && (mode != clientcmdapi.NeverExecInteractiveMode || !cfg.ExecProvider.StdinUnavailable || timeout != completionTimeout) {
			t.Errorf("completing: interactive mode %s, stdin unavailable %v, timeout %s", mode, cfg.ExecProvider.StdinUnavailable, timeout)
		}
		if !completing && (mode != clientcmdapi.AlwaysExecInteractiveMode || timeout != 0) {
			t.Errorf("not completing: interactive mode %s, timeout %s", mode, timeout)
		}
	}
}
