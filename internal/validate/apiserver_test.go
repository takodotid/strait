package validate

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// rawDoc is one document of a fixture file as written, with its "# want: " lines.
type rawDoc struct {
	obj   *unstructured.Unstructured
	wants []string
}

func readRawDocs(t *testing.T, path string) []rawDoc {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var docs []rawDoc
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		chunk, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		var d rawDoc
		for _, line := range strings.Split(string(chunk), "\n") {
			if w, ok := strings.CutPrefix(line, "# want: "); ok {
				d.wants = append(d.wants, w)
			}
		}
		var m map[string]any
		if err := yaml.Unmarshal(chunk, &m); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(m) == 0 {
			continue
		}
		d.obj = &unstructured.Unstructured{Object: m}
		docs = append(docs, d)
	}
	return docs
}

// notChecked is how the API server says that it ran no validation rule, as when the object breaks its schema.
const notChecked = "some validation rules were not checked"

// celMessages returns the message of every validation rule of the CRDs.
func celMessages(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, c := range readCRDs(t) {
		walk("spec", c.spec, func(_ string, s map[string]any) {
			rules, _ := s["x-kubernetes-validations"].([]any)
			for _, r := range rules {
				out[r.(map[string]any)["message"].(string)] = true
			}
		})
	}
	if len(out) == 0 {
		t.Fatal("found no rules in the CRDs")
	}
	return out
}

// The CRDs' schemas and validation rules run on a real API server (RFC0002 requirement 8, RFC0006 requirement 6).
// It accepts every good fixture and rejects every bad one. For each error that strait validate gives, the API server
// names the same field, and for a validation rule it gives the rule's message, unless it ran no rule because the
// object breaks its schema. Every validation rule of the CRDs is produced by the API server for some bad fixture.
// The API server words schema errors in its own way, so only the field is compared for them.
// make test sets KUBEBUILDER_ASSETS to envtest's kube-apiserver and etcd.
func TestFixturesOnAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("KUBEBUILDER_ASSETS is not set; CI must run this test with make test")
		}
		t.Skip("KUBEBUILDER_ASSETS is not set; make test sets it")
	}
	// The CRDs that the test installs. Reading them here makes go test's cache see a change to them.
	crds, err := filepath.Glob("../../config/crd/*.yaml")
	if err != nil || len(crds) == 0 {
		t.Fatal("no CRDs in config/crd")
	}
	for _, p := range crds {
		if _, err := os.ReadFile(p); err != nil {
			t.Fatal(err)
		}
	}
	rules := celMessages(t)
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../config/crd"}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start the API server: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop the API server: %v", err)
		}
	})
	c, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app"}}); err != nil {
		t.Fatal(err)
	}

	for _, path := range files(t, "good") {
		for _, d := range readRawDocs(t, path) {
			t.Run("good/"+filepath.Base(path)+"/"+d.obj.GetName(), func(t *testing.T) {
				if err := c.Create(ctx, d.obj, client.DryRunAll); err != nil {
					t.Errorf("the API server rejects it: %v", err)
				}
			})
		}
	}
	produced := map[string]bool{}
	for _, path := range files(t, "bad") {
		typed, raw := readDocs(t, path), readRawDocs(t, path)
		if len(typed) != len(raw) {
			t.Fatalf("%s: %d Strait objects, %d documents", path, len(typed), len(raw))
		}
		for i, d := range raw {
			t.Run("bad/"+filepath.Base(path)+"/"+d.obj.GetName(), func(t *testing.T) {
				err := c.Create(ctx, d.obj, client.DryRunAll)
				if err == nil {
					t.Fatalf("the API server accepts it; strait validate gives:\n%s", strings.Join(d.wants, "\n"))
				}
				got := err.Error()
				for _, e := range ObjectWith(typed[i].obj, typed[i].fields) {
					switch {
					case strings.HasPrefix(e.Field, "metadata."):
						// The API server's own checks of object metadata, not a rule of the CRDs.
					case rules[e.Detail]:
						if strings.Contains(got, e.Detail) {
							produced[e.Detail] = true
						} else if !strings.Contains(got, notChecked) {
							t.Errorf("the API server does not give %q:\n%s", e.Detail, got)
						}
					case !strings.Contains(got, e.Field+": "):
						t.Errorf("the API server does not name %s, for %q:\n%s", e.Field, e.Error(), got)
					}
				}
			})
		}
	}
	for m := range rules {
		if !produced[m] {
			t.Errorf("no bad fixture makes the API server give %q", m)
		}
	}
}
