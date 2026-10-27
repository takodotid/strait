package cli

import (
	_ "embed"
	"io"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

//go:embed skill.md.tmpl
var skillText string

var skillTemplate = template.Must(template.New("skill").Parse(skillText))

type skillCommand struct {
	Path    string
	Usage   string
	Short   string
	Long    string
	Example string
	Flags   string
}

// Visible reports whether a command belongs in the help, the man pages and the skill document.
func Visible(c *cobra.Command) bool {
	return c.IsAvailableCommand() && !c.IsAdditionalHelpTopicCommand()
}

// WriteSkill writes the skill document of the command tree under root.
func WriteSkill(w io.Writer, root *cobra.Command) error {
	var commands []skillCommand
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if !Visible(c) {
			return
		}
		flags := c.LocalNonPersistentFlags()
		if !c.HasParent() {
			flags = c.LocalFlags()
		}
		commands = append(commands, skillCommand{
			Path:    c.CommandPath(),
			Usage:   c.UseLine(),
			Short:   c.Short,
			Long:    c.Long,
			Example: c.Example,
			Flags:   flagUsages(flags),
		})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	return skillTemplate.Execute(w, map[string]any{
		"Commands": commands,
		"Reasons":  sortedReasons(),
	})
}

// flagUsages returns the usage of every flag but help.
func flagUsages(fs *pflag.FlagSet) string {
	out := pflag.NewFlagSet("", pflag.ContinueOnError)
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Name != "help" {
			out.AddFlag(f)
		}
	})
	return strings.TrimRight(out.FlagUsages(), "\n")
}
