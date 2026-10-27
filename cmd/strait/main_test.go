package main

import "testing"

func TestName(t *testing.T) {
	for path, want := range map[string]string{
		"strait":                        "strait",
		"/usr/local/bin/strait":         "strait",
		"/usr/local/bin/kubectl-strait": "kubectl strait",
		"bin/kubectl-strait.exe":        "kubectl strait",
		"./kubectl-strait-dev":          "strait",
	} {
		if got := name(path); got != want {
			t.Errorf("%s: got %q, want %q", path, got, want)
		}
	}
}
