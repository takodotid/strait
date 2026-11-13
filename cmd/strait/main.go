// Command strait reads the state of Strait at one site and explains it. Installed as kubectl-strait, it runs as kubectl strait.
package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/takodotid/strait/internal/cli"
)

func main() {
	name, args := command(os.Args[0], os.Args[1:])
	os.Exit(cli.Main(name, args, os.Stdin, os.Stdout, os.Stderr))
}

// command returns the command shown in usage lines for the binary at path, and the arguments to run. Run as
// kubectl_complete-strait, the binary answers kubectl's completion of kubectl strait: kubectl passes the words after
// "kubectl strait", and reads the answer of cobra's __complete.
func command(path string, args []string) (string, []string) {
	switch strings.TrimSuffix(filepath.Base(path), ".exe") {
	case "kubectl-strait":
		return "kubectl strait", args
	case "kubectl_complete-strait":
		return "kubectl strait", append([]string{cobra.ShellCompRequestCmd}, args...)
	}
	return "strait", args
}
