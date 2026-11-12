package main

import (
	"slices"
	"testing"
)

func TestCommand(t *testing.T) {
	for path, want := range map[string]string{
		"strait":                                 "strait",
		"/usr/local/bin/strait":                  "strait",
		"/usr/local/bin/kubectl-strait":          "kubectl strait",
		"bin/kubectl-strait.exe":                 "kubectl strait",
		"./kubectl-strait-dev":                   "strait",
		"/usr/local/bin/kubectl_complete-strait": "kubectl strait",
	} {
		if got, _ := command(path, nil); got != want {
			t.Errorf("%s: got %q, want %q", path, got, want)
		}
	}
	// kubectl passes the words after "kubectl strait"; the binary answers as cobra's __complete.
	if _, args := command("kubectl_complete-strait.exe", []string{"explain", "p"}); !slices.Equal(args, []string{"__complete", "explain", "p"}) {
		t.Errorf("kubectl_complete-strait runs %v", args)
	}
	if _, args := command("kubectl-strait", []string{"explain", "p"}); !slices.Equal(args, []string{"explain", "p"}) {
		t.Errorf("kubectl-strait runs %v", args)
	}
}
