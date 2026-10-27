package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/takodotid/strait/internal/cli"
)

// Every command of the tree has a man page and a section of the skill document.
func TestEveryCommandIsGenerated(t *testing.T) {
	dir := t.TempDir()
	skill := filepath.Join(dir, "SKILL.md")
	if err := run([]string{"-skill", skill, filepath.Join(dir, "man")}, io.Discard); err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile(skill)
	if err != nil {
		t.Fatal(err)
	}

	var cmds []*cobra.Command
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if !cli.Visible(c) {
			return
		}
		cmds = append(cmds, c)
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(cli.NewRoot("strait"))
	if len(cmds) < 2 {
		t.Fatalf("walked %d commands", len(cmds))
	}

	pages, err := filepath.Glob(filepath.Join(dir, "man", "*.1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != len(cmds) {
		t.Errorf("%d man pages for %d commands: %v", len(pages), len(cmds), pages)
	}
	for _, c := range cmds {
		path := c.CommandPath()
		page, err := os.ReadFile(filepath.Join(dir, "man", strings.ReplaceAll(path, " ", "-")+".1"))
		if err != nil {
			t.Errorf("%s: no man page: %v", path, err)
		} else if !strings.Contains(string(page), "\n"+strings.ReplaceAll(path, " ", "-")+" - ") || !strings.Contains(string(page), c.UseLine()) {
			t.Errorf("%s: the man page does not hold its name and the usage line %q", path, c.UseLine())
		}
		if !strings.Contains(string(doc), "\n### "+path+"\n") {
			t.Errorf("%s: no section in the skill document", path)
		}
	}
}

func TestUsage(t *testing.T) {
	if err := run(nil, io.Discard); err == nil {
		t.Error("no directory must be an error")
	}
}
