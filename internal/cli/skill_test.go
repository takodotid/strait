package cli

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"sigs.k8s.io/yaml"
)

func skill(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := WriteSkill(&b, NewRoot("strait")); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestSkillFrontmatter(t *testing.T) {
	s := skill(t)
	rest, ok := strings.CutPrefix(s, "---\n")
	if !ok {
		t.Fatalf("the skill must start with ---, got %q", s[:20])
	}
	front, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		t.Fatal("no end of frontmatter")
	}
	var meta map[string]any
	if err := yaml.UnmarshalStrict([]byte(front), &meta); err != nil {
		t.Fatalf("frontmatter is not YAML: %v", err)
	}
	if meta["name"] != "strait" {
		t.Errorf("name: got %v", meta["name"])
	}
	desc, _ := meta["description"].(string)
	if desc == "" || strings.Contains(desc, "\n") || len(desc) > 1024 {
		t.Errorf("description must be one line of at most 1024 characters, got %q", desc)
	}
	if len(meta) != 2 {
		t.Errorf("frontmatter holds more than name and description: %v", meta)
	}
	if !strings.HasPrefix(strings.TrimLeft(body, "\n"), "# Strait\n") {
		t.Errorf("body starts with %q", body[:20])
	}
}

func TestSkillContent(t *testing.T) {
	s := skill(t)
	for _, want := range []string{
		"virtual service between Kubernetes clusters",
		"Strait never changes a workload.",
		"Failover, switchover and promotion belong to the workload's own controller",
		"The `strait` CLI is read-only.",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the skill does not say %q", want)
		}
	}
	for _, r := range sortedReasons() {
		if !strings.Contains(s, "| `"+r.Name+"` | "+r.Meaning+" |\n") {
			t.Errorf("the skill does not explain reason %s", r.Name)
		}
	}
	var again bytes.Buffer
	if err := WriteSkill(&again, NewRoot("strait")); err != nil || again.String() != s {
		t.Error("the skill is not the same on every run")
	}
}

// commands returns every command of the tree that the help shows.
func commands(c *cobra.Command) []*cobra.Command {
	if !Visible(c) {
		return nil
	}
	out := []*cobra.Command{c}
	for _, sub := range c.Commands() {
		out = append(out, commands(sub)...)
	}
	return out
}

func TestEveryCommandIsDocumented(t *testing.T) {
	root := NewRoot("strait")
	cmds := commands(root)
	if len(cmds) != 6 {
		t.Errorf("got %d commands", len(cmds))
	}
	for _, c := range cmds {
		if c.Short == "" || c.Long == "" || c.Example == "" {
			t.Errorf("%s: Short, Long and Example must all be set", c.CommandPath())
		}
		if strings.HasSuffix(c.Short, ".") {
			t.Errorf("%s: Short ends with a period", c.CommandPath())
		}
		for _, text := range []string{c.Short, c.Long, c.Example, c.Use} {
			if strings.ContainsAny(text, "\u2014\u2013") {
				t.Errorf("%s: help text holds a dash that is not ASCII", c.CommandPath())
			}
		}
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if strings.ContainsAny(f.Usage, "\u2014\u2013") {
				t.Errorf("%s --%s: usage holds a dash that is not ASCII", c.CommandPath(), f.Name)
			}
		})
	}
	if strings.ContainsAny(skill(t), "\u2014\u2013") {
		t.Error("the skill holds a dash that is not ASCII")
	}
}

// reasons explains every route reason of api/v1, and no other.
func TestReasonsCoverAPI(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "../../api/v1/siteservice_types.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var api []string
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if !strings.HasPrefix(name.Name, "Reason") || i >= len(spec.Values) {
				continue
			}
			if lit, ok := spec.Values[i].(*ast.BasicLit); ok {
				v, _ := strconv.Unquote(lit.Value)
				api = append(api, v)
			}
		}
		return true
	})
	if len(api) == 0 {
		t.Fatal("found no reason constants")
	}
	slices.Sort(api)
	var have []string
	for _, r := range sortedReasons() {
		have = append(have, r.Name)
	}
	if !slices.Equal(api, have) {
		t.Errorf("api/v1 has reasons %v, the CLI explains %v", api, have)
	}
}
