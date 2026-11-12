package cli

import (
	"bufio"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/json"
	"sigs.k8s.io/yaml"

	straitv1 "github.com/takodotid/strait/api/v1"
	"github.com/takodotid/strait/internal/validate"
)

type problemView struct {
	File     string `json:"file"`
	Document int    `json:"document"`
	// Item is the path of the object in a List document, such as items[2]. Empty for an object of its own.
	Item     string            `json:"item,omitempty"`
	Object   validate.Ref      `json:"object"`
	Severity validate.Severity `json:"severity"`
	Field    string            `json:"field,omitempty"`
	Message  string            `json:"message"`
	// order is the position of the document among all documents read.
	order int
}

type validateView struct {
	// Objects is the number of Strait objects checked.
	Objects int `json:"objects"`
	// Skipped is the number of documents of other API groups.
	Skipped  int           `json:"skipped"`
	Errors   int           `json:"errors"`
	Warnings int           `json:"warnings"`
	Problems []problemView `json:"problems"`
}

func newValidate(o *options) *cobra.Command {
	var paths []string
	cmd := &cobra.Command{
		Use:   "validate -f PATH",
		Short: "Check Strait manifests offline, before they are applied",
		Long: `validate checks Strait manifests with the rules that the CRDs apply. It needs no cluster.

PATH is a file, a directory, or - for standard input. -f may be repeated. A directory is read recursively, for files that end in .yaml, .yml or .json. A file may hold many YAML documents. A document of kind List, as kubectl get -o yaml writes, counts as each of its items.

Each strait.tako.id object is decoded strictly, so an unknown field is an error. Defaults are set before the checks, as the API server sets them: to the fields that the object leaves out, and not to those it sets, also to 0 or 0s. Documents of other API groups are skipped.

The objects are then checked together: at most one Site, a SitePeer for the peer of every SiteLink, spec.wireguard on the Site and on the SitePeer of every wireguard SiteLink, and a distinct listen port for each wireguard SiteLink to one peer. A rule that needs an object missing from the input is a warning.

A direct SiteLink with endpointDirect and no endpointCIDRs is a warning: endpoint-direct then points application names at any address that the peer's document gives, apart from unspecified, loopback, link-local and multicast ones. A peer, or anyone on the path of the link, could pick that address. In production, set endpointCIDRs to the peer's pod CIDRs.

Every problem names its file, document, object and field. The exit status is 1 when there is an error. Warnings alone exit with 0.`,
		Example: `  # Check every manifest under a directory.
  strait validate -f deploy/site-a/

  # Check what a tool renders, from standard input.
  kustomize build deploy/site-a | strait validate -f -`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(paths) == 0 {
				return usageError{errors.New("-f is required")}
			}
			v, err := validateFiles(paths, cmd.InOrStdin())
			if err != nil {
				return err
			}
			if err := o.write(cmd.OutOrStdout(), v, func(w io.Writer) { validateTable(w, v) }); err != nil {
				return err
			}
			if v.Errors > 0 {
				return errFailed
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVarP(&paths, "filename", "f", nil, "A file, a directory, or - for standard input. Repeat it for more.")
	return cmd
}

// document is one YAML document of a file, or one item of a List document.
type document struct {
	file  string
	index int
	// item is the path of the item in its List document, empty for a document of its own.
	item string
	data []byte
}

// isList reports whether a document of this API version and kind is a list of objects: a List, as kubectl get -o
// yaml writes, or a list kind of Strait's API group.
func isList(gv schema.GroupVersion, kind string) bool {
	return (gv.Group == "" && kind == "List") || (gv.Group == straitv1.GroupVersion.Group && strings.HasSuffix(kind, "List"))
}

// expand returns the document, or each item of a List document, of a List in it too.
func expand(d document) ([]document, error) {
	var h header
	if err := yaml.Unmarshal(d.data, &h); err != nil {
		return []document{d}, nil
	}
	gv, err := schema.ParseGroupVersion(h.APIVersion)
	if err != nil || !isList(gv, h.Kind) {
		return []document{d}, nil
	}
	var list struct {
		Items []stdjson.RawMessage `json:"items"`
	}
	if err := yaml.Unmarshal(d.data, &list); err != nil {
		return nil, fmt.Errorf("items of %s: %w", h.Kind, err)
	}
	var out []document
	for i, item := range list.Items {
		sub := document{file: d.file, index: d.index, item: fmt.Sprintf("%sitems[%d]", prefix(d.item), i), data: item}
		docs, err := expand(sub)
		if err != nil {
			return nil, err
		}
		out = append(out, docs...)
	}
	return out, nil
}

func prefix(item string) string {
	if item == "" {
		return ""
	}
	return item + "."
}

// readDocuments reads every document of the paths. A directory is walked for .yaml, .yml and .json files.
func readDocuments(paths []string, stdin io.Reader) ([]document, error) {
	var docs []document
	read := func(name string, r io.Reader) error {
		yr := utilyaml.NewYAMLReader(bufio.NewReader(r))
		for i := 1; ; i++ {
			data, err := yr.Read()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("read %s: %w", name, err)
			}
			docs = append(docs, document{file: name, index: i, data: data})
		}
	}
	readFile := func(path string) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return read(path, f)
	}
	for _, p := range paths {
		if p == "-" {
			if err := read("-", stdin); err != nil {
				return nil, err
			}
			continue
		}
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if err := readFile(p); err != nil {
				return nil, err
			}
			continue
		}
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			switch filepath.Ext(path) {
			case ".yaml", ".yml", ".json":
				return readFile(path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return docs, nil
}

var scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := straitv1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}()

// header is what is read from a document before it is decoded strictly, so that every problem can name its object.
type header struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
}

// decode returns the Strait object of a document and the fields it sets, nil for an empty document or one of another
// API group. An object with unknown or duplicate fields is returned with one error for each.
// An object of the deprecated version is decoded as v1, which has the same schema, with a warning.
func decode(d document) (runtime.Object, validate.Fields, validate.Ref, []error, []string) {
	obj, ref, errs, warnings := decodeObject(d)
	if obj == nil {
		return nil, nil, ref, errs, warnings
	}
	data, err := yaml.YAMLToJSON(d.data)
	if err != nil {
		return nil, nil, ref, []error{err}, warnings
	}
	fields, err := validate.FieldsOf(data)
	if err != nil {
		return nil, nil, ref, []error{err}, warnings
	}
	return obj, fields, ref, errs, warnings
}

func decodeObject(d document) (runtime.Object, validate.Ref, []error, []string) {
	var empty any
	if err := yaml.Unmarshal(d.data, &empty); err != nil {
		return nil, validate.Ref{}, []error{err}, nil
	}
	if empty == nil {
		return nil, validate.Ref{}, nil, nil
	}
	var h header
	if err := yaml.Unmarshal(d.data, &h); err != nil {
		return nil, validate.Ref{}, []error{err}, nil
	}
	ref := validate.Ref{Kind: h.Kind, Namespace: h.Metadata.Namespace, Name: h.Metadata.Name}
	if h.APIVersion == "" || h.Kind == "" {
		return nil, ref, []error{errors.New("apiVersion and kind are required")}, nil
	}
	gv, err := schema.ParseGroupVersion(h.APIVersion)
	if err != nil {
		return nil, ref, []error{err}, nil
	}
	if gv.Group != straitv1.GroupVersion.Group {
		return nil, ref, nil, nil
	}
	var warnings []string
	if gv == straitv1.DeprecatedGroupVersion {
		warnings = append(warnings, fmt.Sprintf("%s is deprecated; use %s", gv, straitv1.GroupVersion))
		gv = straitv1.GroupVersion
	}
	obj, err := scheme.New(gv.WithKind(h.Kind))
	if err != nil {
		return nil, ref, []error{fmt.Errorf("%s is not a kind of %s", h.Kind, straitv1.GroupVersion)}, nil
	}
	data, err := yaml.YAMLToJSONStrict(d.data)
	if err != nil {
		return nil, ref, []error{err}, nil
	}
	strict, err := json.UnmarshalStrict(data, obj, json.DisallowDuplicateFields, json.DisallowUnknownFields)
	if err != nil {
		return nil, ref, []error{err}, nil
	}
	obj.GetObjectKind().SetGroupVersionKind(gv.WithKind(h.Kind))
	return obj, ref, strict, warnings
}

func validateFiles(paths []string, stdin io.Reader) (validateView, error) {
	v := validateView{Problems: []problemView{}}
	read, err := readDocuments(paths, stdin)
	if err != nil {
		return v, err
	}
	var docs []document
	for _, d := range read {
		items, err := expand(d)
		if err != nil {
			v.Problems = append(v.Problems, problemView{File: d.file, Document: d.index, Item: d.item, Severity: validate.SeverityError, Message: err.Error(), order: len(docs)})
			docs = append(docs, document{file: d.file, index: d.index})
			continue
		}
		docs = append(docs, items...)
	}
	var objs []runtime.Object
	var fields []validate.Fields
	var sources []int
	for i, d := range docs {
		if d.data == nil {
			continue
		}
		obj, f, ref, errs, warnings := decode(d)
		for _, err := range errs {
			v.Problems = append(v.Problems, problemView{File: d.file, Document: d.index, Item: d.item, Object: ref, Severity: validate.SeverityError, Message: err.Error(), order: i})
		}
		for _, w := range warnings {
			v.Problems = append(v.Problems, problemView{File: d.file, Document: d.index, Item: d.item, Object: ref, Severity: validate.SeverityWarning, Field: "apiVersion", Message: w, order: i})
		}
		switch {
		case obj != nil:
			objs = append(objs, obj)
			fields = append(fields, f)
			sources = append(sources, i)
		case ref.Kind != "" && len(errs) == 0:
			v.Skipped++
		}
	}
	v.Objects = len(objs)
	for _, p := range validate.SetWith(objs, fields) {
		i := sources[p.Index]
		v.Problems = append(v.Problems, problemView{File: docs[i].file, Document: docs[i].index, Item: docs[i].item, Object: p.Object, Severity: p.Severity, Field: p.Field, Message: p.Message, order: i})
	}
	sort.SliceStable(v.Problems, func(a, b int) bool { return v.Problems[a].order < v.Problems[b].order })
	for _, p := range v.Problems {
		if p.Severity == validate.SeverityError {
			v.Errors++
		} else {
			v.Warnings++
		}
	}
	return v, nil
}

func validateTable(w io.Writer, v validateView) {
	if len(v.Problems) > 0 {
		var rows [][]string
		for _, p := range v.Problems {
			obj := p.Object.String()
			if p.Object.Kind == "" {
				obj = "-"
			}
			doc := number(p.Document)
			if p.Item != "" {
				doc += " " + p.Item
			}
			rows = append(rows, []string{string(p.Severity), p.File, doc, obj, dash(p.Field), p.Message})
		}
		table(w, []string{"SEVERITY", "FILE", "DOC", "OBJECT", "FIELD", "MESSAGE"}, rows)
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "Objects checked: %d. Documents of other API groups skipped: %d. Errors: %d. Warnings: %d.\n", v.Objects, v.Skipped, v.Errors, v.Warnings)
}
