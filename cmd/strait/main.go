// Command strait reads the state of Strait at one site and explains it. Installed as kubectl-strait, it runs as kubectl strait.
package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/takodotid/strait/internal/cli"
)

func main() {
	os.Exit(cli.Main(name(os.Args[0]), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// name returns the command shown in usage lines for the binary at path.
func name(path string) string {
	if strings.TrimSuffix(filepath.Base(path), ".exe") == "kubectl-strait" {
		return "kubectl strait"
	}
	return "strait"
}
