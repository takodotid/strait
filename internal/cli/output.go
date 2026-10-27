package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
	"sigs.k8s.io/yaml"
)

// write prints view as JSON or YAML, or calls table for the table format.
func (o *options) write(w io.Writer, view any, table func(io.Writer)) error {
	switch o.output {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(view)
	case "yaml":
		data, err := yaml.Marshal(view)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	table(w)
	return nil
}

// table prints rows under a header, aligned in columns.
func table(w io.Writer, header []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

// fields prints name and value pairs, aligned.
func fields(w io.Writer, pairs ...[2]string) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, p := range pairs {
		fmt.Fprintf(tw, "%s:\t%s\n", p[0], p[1])
	}
	tw.Flush()
}

// age returns how long ago t was, or - when t is not set.
func (o *options) age(t *metav1.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return duration.HumanDuration(o.now().Sub(t.Time))
}

// when returns t and how long ago it was, or - when t is not set.
func (o *options) when(t *metav1.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return fmt.Sprintf("%s (%s ago)", t.UTC().Format(time.RFC3339), o.age(t))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func list(s []string) string {
	return dash(strings.Join(s, ","))
}

func boolean(b bool) string {
	return strconv.FormatBool(b)
}

func number[T ~int32 | ~int64 | ~int](n T) string {
	return strconv.FormatInt(int64(n), 10)
}
