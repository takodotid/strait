package validate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	straitv1 "github.com/takodotid/strait/api/v1"
)

type doc struct {
	obj   runtime.Object
	wants []string
}

var scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := straitv1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}()

// readDocs decodes the Strait objects of a multi-document file, with the "# want: " lines of each document.
func readDocs(t *testing.T, path string) []doc {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var docs []doc
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		chunk, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		var d doc
		for _, line := range strings.Split(string(chunk), "\n") {
			if w, ok := strings.CutPrefix(line, "# want: "); ok {
				d.wants = append(d.wants, w)
			}
		}
		var tm metav1.TypeMeta
		if err := yaml.Unmarshal(chunk, &tm); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		gvk := schema.FromAPIVersionAndKind(tm.APIVersion, tm.Kind)
		if gvk.Group != straitv1.GroupVersion.Group {
			if len(d.wants) > 0 {
				t.Fatalf("%s: wants on a document that is not a Strait object", path)
			}
			continue
		}
		obj, err := scheme.New(gvk)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if err := yaml.UnmarshalStrict(chunk, obj); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		d.obj = obj
		docs = append(docs, d)
	}
	return docs
}

func files(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", dir, "*.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures in testdata/%s", dir)
	}
	return paths
}

func strs(errs []error) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Error()
	}
	slices.Sort(out)
	return out
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

func TestGood(t *testing.T) {
	for _, path := range files(t, "good") {
		for _, d := range readDocs(t, path) {
			t.Run(filepath.Base(path)+"/"+RefOf(d.obj).String(), func(t *testing.T) {
				if errs := Object(d.obj); len(errs) > 0 {
					t.Errorf("want no errors, got:\n%s", strings.Join(strs(errs.ToAggregate().Errors()), "\n"))
				}
			})
		}
	}
}

func TestBad(t *testing.T) {
	for _, path := range files(t, "bad") {
		for _, d := range readDocs(t, path) {
			t.Run(filepath.Base(path)+"/"+RefOf(d.obj).String(), func(t *testing.T) {
				if len(d.wants) == 0 {
					t.Fatal("a bad fixture needs at least one want line")
				}
				before := d.obj.DeepCopyObject()
				var got []string
				for _, e := range Object(d.obj) {
					got = append(got, e.Error())
				}
				if !slices.Equal(sorted(got), sorted(d.wants)) {
					t.Errorf("got:\n%s\nwant:\n%s", strings.Join(sorted(got), "\n"), strings.Join(sorted(d.wants), "\n"))
				}
				if !reflect.DeepEqual(before, d.obj) {
					t.Error("Object changed its input")
				}
			})
		}
	}
}

func TestSet(t *testing.T) {
	for _, path := range files(t, "set") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			docs := readDocs(t, path)
			var objs []runtime.Object
			var wants []string
			for _, d := range docs {
				objs = append(objs, d.obj)
				wants = append(wants, d.wants...)
			}
			problems := Set(objs)
			var got []string
			for i, p := range problems {
				got = append(got, p.String())
				if RefOf(objs[p.Index]) != p.Object {
					t.Errorf("problem %q has index %d, which is %s", p, p.Index, RefOf(objs[p.Index]))
				}
				if i > 0 && problems[i-1].Index > p.Index {
					t.Errorf("problems are not ordered by object")
				}
			}
			if !slices.Equal(sorted(got), sorted(wants)) {
				t.Errorf("got:\n%s\nwant:\n%s", strings.Join(sorted(got), "\n"), strings.Join(sorted(wants), "\n"))
			}
		})
	}
}

func TestObjectUnknownKind(t *testing.T) {
	errs := Object(&straitv1.SiteList{})
	if len(errs) != 1 || errs[0].Field != "kind" {
		t.Errorf("got %v", errs)
	}
}

func TestDefaultLeavesSetValues(t *testing.T) {
	l := &straitv1.SiteLink{Spec: straitv1.SiteLinkSpec{Cost: 7, WireGuard: &straitv1.WireGuardLink{ListenPort: 51900}}}
	Default(l)
	if l.Spec.Cost != 7 || l.Spec.WireGuard.ListenPort != 51900 || l.Spec.WireGuard.PersistentKeepalive.Duration.String() != "25s" {
		t.Errorf("got %+v", l.Spec)
	}
}

// crd is the part of a generated CRD that the parity tests read.
type crd struct {
	kind string
	spec map[string]any
}

func readCRDs(t *testing.T) []crd {
	t.Helper()
	paths, err := filepath.Glob("../../config/crd/*.yaml")
	if err != nil || len(paths) == 0 {
		t.Fatal("no CRDs in config/crd")
	}
	var out []crd
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			Spec struct {
				Names    struct{ Kind string }
				Versions []struct {
					Schema struct {
						OpenAPIV3Schema struct {
							Properties map[string]map[string]any
						}
					}
				}
			}
		}
		if err := yaml.Unmarshal(data, &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, crd{kind: c.Spec.Names.Kind, spec: c.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]})
	}
	return out
}

// walk calls fn for every schema under s, with its path. It does not enter arrays.
func walk(path string, s map[string]any, fn func(path string, s map[string]any)) {
	fn(path, s)
	props, _ := s["properties"].(map[string]any)
	for name, p := range props {
		walk(path+"."+name, p.(map[string]any), fn)
	}
	if items, ok := s["items"].(map[string]any); ok {
		walk(path+"[]", items, fn)
	}
}

// Every XValidation message of the CRDs is produced by a bad fixture.
func TestEveryRuleHasAFixture(t *testing.T) {
	produced := map[string]bool{}
	for _, path := range files(t, "bad") {
		for _, d := range readDocs(t, path) {
			for _, e := range Object(d.obj) {
				produced[e.Detail] = true
			}
		}
	}
	n := 0
	for _, c := range readCRDs(t) {
		walk("spec", c.spec, func(path string, s map[string]any) {
			rules, _ := s["x-kubernetes-validations"].([]any)
			for _, r := range rules {
				msg := r.(map[string]any)["message"].(string)
				n++
				if !produced[msg] {
					t.Errorf("%s %s: no fixture produces %q", c.kind, path, msg)
				}
			}
		})
	}
	if n == 0 {
		t.Fatal("found no rules in the CRDs")
	}
}

// Every pattern of the CRDs is one that this package checks.
func TestPatterns(t *testing.T) {
	known := []string{dnsLabel.String(), wireGuardKey.String()}
	for _, c := range readCRDs(t) {
		walk("spec", c.spec, func(path string, s map[string]any) {
			if p, ok := s["pattern"].(string); ok && !slices.Contains(known, p) {
				t.Errorf("%s %s: pattern %q is not checked", c.kind, path, p)
			}
		})
	}
}

// Default sets every default of the CRDs, to the same value.
func TestDefaultsMatchCRDs(t *testing.T) {
	// Every optional struct is set, so that the defaults inside them apply.
	objs := map[string]runtime.Object{
		"Site":        &straitv1.Site{Spec: straitv1.SiteSpec{WireGuard: &straitv1.SiteWireGuard{}}},
		"SitePeer":    &straitv1.SitePeer{},
		"SiteLink":    &straitv1.SiteLink{Spec: straitv1.SiteLinkSpec{WireGuard: &straitv1.WireGuardLink{}}},
		"Export":      &straitv1.Export{},
		"Import":      &straitv1.Import{},
		"SiteService": &straitv1.SiteService{},
	}
	n := 0
	for _, c := range readCRDs(t) {
		obj, ok := objs[c.kind]
		if !ok {
			t.Fatalf("no object for kind %s", c.kind)
		}
		Default(obj)
		data, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		walk("spec", c.spec, func(path string, s map[string]any) {
			want, ok := s["default"]
			if !ok {
				return
			}
			n++
			v, found := lookup(got, path)
			if !found {
				t.Errorf("%s %s: default %v is not set", c.kind, path, want)
				return
			}
			if m, ok := want.(map[string]any); ok {
				for k, w := range m {
					if !reflect.DeepEqual(v.(map[string]any)[k], normalize(w)) {
						t.Errorf("%s %s.%s: got %v, want %v", c.kind, path, k, v.(map[string]any)[k], w)
					}
				}
				return
			}
			if !reflect.DeepEqual(v, normalize(want)) {
				t.Errorf("%s %s: got %v, want %v", c.kind, path, v, want)
			}
		})
	}
	if n == 0 {
		t.Fatal("found no defaults in the CRDs")
	}
}

func lookup(m map[string]any, path string) (any, bool) {
	var v any = m
	for _, k := range strings.Split(path, ".") {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = mm[k]; !ok {
			return nil, false
		}
	}
	return v, true
}

// normalize gives a value parsed from YAML the types that encoding/json gives.
func normalize(v any) any {
	data, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(data, &out)
	return out
}
