package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	straitv1 "github.com/takodotid/strait/api/v1"
)

// onTerminal runs the command line as cli does, with stdout a terminal of the given width (0: unknown).
func onTerminal(t *testing.T, r client.Reader, width int, args ...string) result {
	t.Helper()
	o := &options{
		connect:  func(*options) (client.Reader, string, error) { return r, "app", nil },
		now:      func() time.Time { return now },
		terminal: func(io.Writer) (bool, int) { return true, width },
	}
	var out, errOut bytes.Buffer
	code := run(newRoot("strait", o), args, strings.NewReader(""), &out, &errOut)
	return result{code, out.String(), errOut.String()}
}

// On a terminal, explain has colours, a symbol before the route and box tables. With the colours taken out, the text
// is the NO_COLOR output, and every status and reason name is there unchanged.
func TestExplainOnATerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	colour := onTerminal(t, fakeReader(cluster()...), 0, "explain", "pg").ok(t)
	for _, want := range []string{
		"\x1b[32m✔\x1b[39m \x1b[32m\x1b[1mpg-a\x1b[22m\x1b[39m",
		"\x1b[32m\x1b[1mActive\x1b[22m\x1b[39m",
		"\x1b[1mMembers:\x1b[22m",
		"\x1b[2m┌",
		"\x1b[1mNAME\x1b[22m",
		"\x1b[32mtrue\x1b[39m",
		"\x1b[32mTrue\x1b[39m",
	} {
		if !strings.Contains(colour.stdout, want) {
			t.Errorf("no %q in:\n%s", want, colour.stdout)
		}
	}

	t.Setenv("NO_COLOR", "1")
	plain := onTerminal(t, fakeReader(cluster()...), 0, "explain", "pg").ok(t)
	if strings.Contains(plain.stdout, "\x1b[") {
		t.Errorf("NO_COLOR: a colour in:\n%s", plain.stdout)
	}
	diff(t, ansi.ReplaceAllString(colour.stdout, ""), plain.stdout)
	for _, want := range []string{
		"Route:        ✔ pg-a\n",
		"Reason:       Active\n",
		"│ pg-a │ a    │ true   │ Local │ primary │ true     │ Stable │ PrimaryCluster │       1s │        1s │\n",
		"│ Multiple │ False  │ OneWritable │   60m │ -          │\n",
	} {
		if !strings.Contains(plain.stdout, want) {
			t.Errorf("NO_COLOR: no %q in:\n%s", want, plain.stdout)
		}
	}
}

// Piped, explain prints plain text: no colour, no symbol and no box, whatever NO_COLOR says. TestExplain holds the
// exact text.
func TestExplainPiped(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	r := cli(t, fakeReader(cluster()...), "", "explain", "pg").ok(t)
	for _, not := range []string{"\x1b[", "✔", "■", "▲", "┌", "│"} {
		if strings.Contains(r.stdout, not) {
			t.Errorf("piped output holds %q:\n%s", not, r.stdout)
		}
	}
}

// Machine output stays exact on a terminal.
func TestExplainJSONOnATerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	for _, format := range []string{"json", "yaml"} {
		term := onTerminal(t, fakeReader(cluster()...), 80, "explain", "pg", "-o", format).ok(t)
		piped := cli(t, fakeReader(cluster()...), "", "explain", "pg", "-o", format).ok(t)
		if term.stdout != piped.stdout {
			t.Errorf("-o %s differs on a terminal:\n%s", format, term.stdout)
		}
	}
}

// The symbol and the colour of a route: confirmed, kept while Unknown or awaiting peers, and nowhere.
func TestRouteLook(t *testing.T) {
	st := style{terminal: true, color: true}
	for _, tt := range []struct {
		members []string
		reason  string
		symbol  string
		code    string
	}{
		{[]string{"pg-a"}, straitv1.ReasonActive, "✔", "32"},
		{[]string{"r-a", "r-b"}, straitv1.ReasonEligible, "✔", "32"},
		{[]string{"pg-a"}, straitv1.ReasonPreferDeclared, "✔", "32"},
		{[]string{"pg-a"}, straitv1.ReasonHeldUnknown, "▲", "33"},
		{[]string{"pg-a"}, straitv1.ReasonAwaitingPeers, "▲", "33"},
		{nil, straitv1.ReasonSiteIsolated, "■", "31"},
		{nil, straitv1.ReasonDeclarationsDisagree, "■", "31"},
	} {
		symbol, paint := routeLook(st, tt.members, tt.reason)
		if symbol != tt.symbol || paint("x") != "\x1b["+tt.code+"mx\x1b[39m" {
			t.Errorf("%v %s: %q %q, want %q in %s", tt.members, tt.reason, symbol, paint("x"), tt.symbol, tt.code)
		}
	}
	if symbol, _ := routeLook(st, nil, ""); symbol != "" {
		t.Errorf("no status yet: symbol %q", symbol)
	}
}

// In a terminal too narrow for a table, its last column wraps, and no line is wider than the terminal when the other
// columns fit.
func TestBoxTableWrapsTheLastColumn(t *testing.T) {
	var out bytes.Buffer
	st := style{terminal: true, width: 40}
	st.boxTable(&out, []column{{title: "TYPE"}, {title: "MESSAGE"}}, [][]string{{"Routed", "routed to pg-a but no endpoint is ready, see the Applied condition"}})
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) < 6 {
		t.Fatalf("the message did not wrap:\n%s", out.String())
	}
	for _, l := range lines {
		if visible(l) > 40 {
			t.Errorf("line of %d columns in a terminal of 40: %q", visible(l), l)
		}
	}
	if !strings.Contains(out.String(), "│ Routed │ routed to pg-a but no") {
		t.Errorf("wrapped at the wrong place:\n%s", out.String())
	}
}

// A condition's status is green when it is as it should be, red when it stops or risks traffic, and yellow for a hold.
func TestConditionPaint(t *testing.T) {
	st := style{terminal: true, color: true}
	for _, tt := range []struct {
		typ    string
		status metav1.ConditionStatus
		code   string
	}{
		{straitv1.ConditionRouted, metav1.ConditionTrue, "32"},
		{straitv1.ConditionRouted, metav1.ConditionFalse, "31"},
		{straitv1.ConditionMultiple, metav1.ConditionTrue, "31"},
		{straitv1.ConditionHeld, metav1.ConditionTrue, "33"},
		{straitv1.ConditionApplied, metav1.ConditionFalse, "31"},
	} {
		if got := conditionPaint(st, metav1.Condition{Type: tt.typ, Status: tt.status})("x"); got != "\x1b["+tt.code+"mx\x1b[39m" {
			t.Errorf("%s %s: %q, want colour %s", tt.typ, tt.status, got, tt.code)
		}
	}
	if got := conditionPaint(st, metav1.Condition{Type: "Other", Status: metav1.ConditionTrue})("x"); got != "x" {
		t.Errorf("an unknown condition is painted: %q", got)
	}
}
