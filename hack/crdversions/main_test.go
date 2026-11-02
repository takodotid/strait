package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"sigs.k8s.io/yaml"
)

// Every generated CRD stores v1 and serves v1alpha1 as a deprecated version with the same schema, so objects that v0.1
// releases wrote keep working, and the API server converts between the two by changing apiVersion only.
func TestGeneratedCRDs(t *testing.T) {
	for _, dir := range []string{"../../config/crd", "../../deploy/helm/strait/crds"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
		if err != nil || len(files) != 6 {
			t.Fatalf("%s: want 6 CRDs, found %d (%v)", dir, len(files), err)
		}
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var crd struct {
				Spec struct {
					Names    struct{ Kind string }
					Versions []map[string]any
				}
			}
			if err := yaml.Unmarshal(data, &crd); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			v := crd.Spec.Versions
			if len(v) != 2 {
				t.Errorf("%s: want versions v1 and v1alpha1, found %d", f, len(v))
				continue
			}
			if v[0]["name"] != "v1" || v[0]["served"] != true || v[0]["storage"] != true || v[0]["deprecated"] != nil {
				t.Errorf("%s: want v1 served and stored, found name %v served %v storage %v deprecated %v", f, v[0]["name"], v[0]["served"], v[0]["storage"], v[0]["deprecated"])
			}
			want := "strait.tako.id/v1alpha1 " + crd.Spec.Names.Kind + " is deprecated; use strait.tako.id/v1"
			if v[1]["name"] != "v1alpha1" || v[1]["served"] != true || v[1]["storage"] != false || v[1]["deprecated"] != true || v[1]["deprecationWarning"] != want {
				t.Errorf("%s: want v1alpha1 served, not stored, deprecated with %q, found %v", f, want, v[1])
			}
			for _, k := range []string{"schema", "subresources", "additionalPrinterColumns"} {
				if !reflect.DeepEqual(v[0][k], v[1][k]) {
					t.Errorf("%s: %s differs between v1 and v1alpha1", f, k)
				}
			}
		}
	}
}

// addDeprecated refuses a CRD it has already rewritten, so a second run cannot add a third version.
func TestAddDeprecatedOnce(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/strait.tako.id_sites.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := addDeprecated(data); err == nil {
		t.Error("a CRD with two versions was rewritten again")
	}
}
