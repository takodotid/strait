package main

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The archive holds the directory itself, every file under it with its content, and names with forward slashes.
func TestWrite(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "strait_v1.1.0_windows_amd64")
	files := map[string]string{
		"strait.exe":          "binary",
		"kubectl-strait.exe":  "binary",
		"SKILL.md":            "---\nname: strait\n---\n",
		"man/man1/strait.1":   ".TH strait\n",
		"man/man1/strait-x.1": ".TH strait-x\n",
	}
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(tmp, "out.zip")
	if err := run([]string{archive, dir + string(filepath.Separator)}); err != nil {
		t.Fatal(err)
	}

	r, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var names []string
	got := map[string]string{}
	for _, f := range r.File {
		names = append(names, f.Name)
		if f.FileInfo().IsDir() {
			continue
		}
		if f.Method != zip.Deflate {
			t.Errorf("%s: method %d, want deflate", f.Name, f.Method)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		got[f.Name] = string(b)
	}
	slices.Sort(names)
	want := []string{
		"strait_v1.1.0_windows_amd64/",
		"strait_v1.1.0_windows_amd64/SKILL.md",
		"strait_v1.1.0_windows_amd64/kubectl-strait.exe",
		"strait_v1.1.0_windows_amd64/man/",
		"strait_v1.1.0_windows_amd64/man/man1/",
		"strait_v1.1.0_windows_amd64/man/man1/strait-x.1",
		"strait_v1.1.0_windows_amd64/man/man1/strait.1",
		"strait_v1.1.0_windows_amd64/strait.exe",
	}
	if !slices.Equal(names, want) {
		t.Errorf("entries:\n got %q\nwant %q", names, want)
	}
	for name, content := range files {
		if got["strait_v1.1.0_windows_amd64/"+name] != content {
			t.Errorf("%s: got %q, want %q", name, got["strait_v1.1.0_windows_amd64/"+name], content)
		}
	}
}

// A failed write leaves no archive behind.
func TestWriteFails(t *testing.T) {
	tmp := t.TempDir()
	archive := filepath.Join(tmp, "out.zip")
	for name, args := range map[string][]string{
		"missing directory": {archive, filepath.Join(tmp, "missing")},
		"usage":             {archive},
	} {
		if err := run(args); err == nil {
			t.Errorf("%s: no error", name)
		}
		if _, err := os.Stat(archive); !os.IsNotExist(err) {
			t.Errorf("%s: archive left behind: %v", name, err)
		}
	}
}
