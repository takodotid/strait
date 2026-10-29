// Command crdversions adds the deprecated v1alpha1 version to each CRD that controller-gen wrote from api/v1.
// Both versions share one schema, so the API server converts between them by changing apiVersion only. v1 is stored.
//
// Usage: crdversions DIR
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"

	straitv1 "github.com/takodotid/strait/api/v1"
)

var (
	group      = straitv1.GroupVersion.Group
	stored     = straitv1.GroupVersion.Version
	deprecated = straitv1.DeprecatedGroupVersion.Version
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: crdversions DIR")
		os.Exit(2)
	}
	files, err := filepath.Glob(filepath.Join(os.Args[1], "*.yaml"))
	if err == nil && len(files) == 0 {
		err = fmt.Errorf("no CRD in %s", os.Args[1])
	}
	for _, f := range files {
		if err != nil {
			break
		}
		err = rewrite(f)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "crdversions:", err)
		os.Exit(1)
	}
}

func rewrite(path string) error {
	in, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := addDeprecated(in)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return os.WriteFile(path, out, 0o644)
}

// addDeprecated takes a CRD that holds the v1 version only, and returns it with v1 stored and a copy of v1 served as
// the deprecated v1alpha1.
func addDeprecated(in []byte) ([]byte, error) {
	var crd map[string]any
	if err := yaml.Unmarshal(in, &crd); err != nil {
		return nil, err
	}
	spec, _ := crd["spec"].(map[string]any)
	names, _ := spec["names"].(map[string]any)
	versions, _ := spec["versions"].([]any)
	if len(versions) != 1 {
		return nil, fmt.Errorf("want one version from controller-gen, found %d", len(versions))
	}
	v1, _ := versions[0].(map[string]any)
	if v1["name"] != stored || spec["group"] != group {
		return nil, fmt.Errorf("want %s/%s, found %v/%v", group, stored, spec["group"], v1["name"])
	}
	v1["served"], v1["storage"] = true, true

	old, err := deepCopy(v1)
	if err != nil {
		return nil, err
	}
	old["name"] = deprecated
	old["storage"] = false
	old["deprecated"] = true
	old["deprecationWarning"] = fmt.Sprintf("%s/%s %v is deprecated; use %s/%s", group, deprecated, names["kind"], group, stored)
	spec["versions"] = []any{v1, old}

	out, err := yaml.Marshal(crd)
	if err != nil {
		return nil, err
	}
	return append([]byte("---\n"), out...), nil
}

func deepCopy(m map[string]any) (map[string]any, error) {
	b, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = yaml.Unmarshal(bytes.TrimSpace(b), &out)
	return out, err
}
