// Command gendocs writes the man pages of strait into a directory, and its skill document when asked.
//
//	go run ./hack/gendocs [-skill <file>] <dir>
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"

	"github.com/takodotid/strait/internal/cli"
)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "gendocs:", err)
		os.Exit(1)
	}
}

func run(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("gendocs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	skill := fs.String("skill", "", "Also write the skill document to this file.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: gendocs [-skill <file>] <dir>")
	}
	dir := fs.Arg(0)

	root := cli.NewRoot("strait")
	var noTag func(c *cobra.Command)
	noTag = func(c *cobra.Command) {
		c.DisableAutoGenTag = true
		for _, sub := range c.Commands() {
			noTag(sub)
		}
	}
	noTag(root)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := doc.GenManTree(root, &doc.GenManHeader{Source: "Strait", Manual: "Strait Manual"}, dir); err != nil {
		return err
	}
	if *skill == "" {
		return nil
	}
	f, err := os.Create(*skill)
	if err != nil {
		return err
	}
	if err := cli.WriteSkill(f, root); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
