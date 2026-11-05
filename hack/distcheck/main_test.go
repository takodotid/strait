package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	version = "v1.3.0"
	commit  = "0123456789abcdef0123456789abcdef01234567"
)

var pages = []string{"strait.1", "strait-status.1", "strait-version.1"}

// fakeBinary is a binary as the fakes of readBuildSettings and runVersion read it: its platform and what it carries.
func fakeBinary(goos, goarch, carries string) []byte {
	return []byte(fmt.Sprintf("GOOS=%s GOARCH=%s %s", goos, goarch, carries))
}

func init() {
	readBuildSettings = func(file string) (map[string]string, error) {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		out := map[string]string{}
		for _, f := range strings.Fields(string(data)) {
			if k, v, ok := strings.Cut(f, "="); ok {
				out[k] = v
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("not a Go binary")
		}
		return out, nil
	}
	runVersion = func(file string) (string, error) {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		f := strings.Fields(string(data))
		return fmt.Sprintf("Version:  %s\nCommit:   %s\n", f[2], f[3]), nil
	}
}

// goodArchive is the content of a platform's archive as make dist writes it.
func goodArchive(goos, goarch string) map[string][]byte {
	dir := fmt.Sprintf("strait_%s_%s_%s/", version, goos, goarch)
	exe := ""
	if goos == "windows" {
		exe = ".exe"
	}
	files := map[string][]byte{
		dir + "strait" + exe:         fakeBinary(goos, goarch, version+" "+commit),
		dir + "kubectl-strait" + exe: fakeBinary(goos, goarch, version+" "+commit),
		dir + "SKILL.md":             []byte("---\nname: strait\n---\n"),
		dir + "LICENSE":              []byte("Apache"),
		dir + "README.md":            []byte("# Strait"),
	}
	for _, p := range pages {
		files[dir+"man/man1/"+p] = []byte(".TH")
	}
	return files
}

// writeArchive writes files into a .tar.gz or a .zip, with an entry for each directory, as tar and hack/zipdir do.
func writeArchive(t *testing.T, file string, files map[string][]byte) {
	t.Helper()
	var buf bytes.Buffer
	dirs := map[string]bool{}
	for name := range files {
		for d := filepath.Dir(name); d != "."; d = filepath.Dir(d) {
			dirs[d+"/"] = true
		}
	}
	if strings.HasSuffix(file, ".zip") {
		zw := zip.NewWriter(&buf)
		for d := range dirs {
			if _, err := zw.Create(d); err != nil {
				t.Fatal(err)
			}
		}
		for name, data := range files {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(data); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for d := range dirs {
			if err := tw.WriteHeader(&tar.Header{Name: d, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
				t.Fatal(err)
			}
		}
		writeFiles(t, tw, files)
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeFiles(t *testing.T, tw *tar.Writer, files map[string][]byte) {
	t.Helper()
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

// manDir writes the man pages that hack/gendocs would write.
func manDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, p := range pages {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(".TH"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func archiveFile(goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("strait_%s_%s_%s%s", version, goos, goarch, ext)
}

// The release's six archives pass. Each archive that lacks a platform, a file, or a man page, holds more, or holds a
// binary of another platform or without the version, fails, and the error names it (RFC0006 requirements 10, 13, 15
// and 16).
func TestArchives(t *testing.T) {
	host := runtime.GOOS + "/" + runtime.GOARCH
	for _, tt := range []struct {
		name string
		// change edits the archives, by platform, before they are written. A nil archive is not written.
		change func(a map[string]map[string][]byte)
		want   string
	}{
		{"good", func(map[string]map[string][]byte) {}, ""},
		{"no darwin/arm64", func(a map[string]map[string][]byte) { a["darwin/arm64"] = nil }, archiveFile("darwin", "arm64") + ": open"},
		{"no man page", func(a map[string]map[string][]byte) {
			delete(a["linux/arm64"], "strait_"+version+"_linux_arm64/man/man1/strait-status.1")
		}, "lacks strait_" + version + "_linux_arm64/man/man1/strait-status.1"},
		{"no LICENSE", func(a map[string]map[string][]byte) {
			delete(a["windows/amd64"], "strait_"+version+"_windows_amd64/LICENSE")
		}, "lacks strait_" + version + "_windows_amd64/LICENSE"},
		{"a file outside the directory", func(a map[string]map[string][]byte) { a["linux/amd64"]["strait"] = []byte("x") }, "holds strait, which is not a file of"},
		{"windows binary without .exe", func(a map[string]map[string][]byte) {
			dir := "strait_" + version + "_windows_arm64/"
			a["windows/arm64"][dir+"strait"] = a["windows/arm64"][dir+"strait.exe"]
			delete(a["windows/arm64"], dir+"strait.exe")
		}, "lacks strait_" + version + "_windows_arm64/strait.exe"},
		{"a binary of another platform", func(a map[string]map[string][]byte) {
			a["darwin/amd64"]["strait_"+version+"_darwin_amd64/kubectl-strait"] = fakeBinary("darwin", "arm64", version+" "+commit)
		}, "kubectl-strait: built for darwin/arm64, want darwin/amd64"},
		{"a binary without the commit", func(a map[string]map[string][]byte) {
			a["linux/arm64"]["strait_"+version+"_linux_arm64/strait"] = fakeBinary("linux", "arm64", version+" unknown")
		}, "strait: does not carry version"},
		{"the host's strait version prints another commit", func(a map[string]map[string][]byte) {
			goos, goarch, _ := strings.Cut(host, "/")
			dir := fmt.Sprintf("strait_%s_%s_%s/", version, goos, goarch)
			if _, ok := a[host]; !ok {
				t.Skip("no archive for the host's platform")
			}
			a[host][dir+"strait"] = fakeBinary(goos, goarch, version+" "+strings.Repeat("f", 40)+" "+commit)
		}, "strait version prints"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			archives := map[string]map[string][]byte{}
			for _, p := range platforms {
				goos, goarch, _ := strings.Cut(p, "/")
				archives[p] = goodArchive(goos, goarch)
			}
			tt.change(archives)
			dist := t.TempDir()
			for _, p := range platforms {
				if archives[p] == nil {
					continue
				}
				goos, goarch, _ := strings.Cut(p, "/")
				writeArchive(t, filepath.Join(dist, archiveFile(goos, goarch)), archives[p])
				// make dist leaves the archive's directory next to it.
				if err := os.Mkdir(filepath.Join(dist, fmt.Sprintf("strait_%s_%s_%s", version, goos, goarch)), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			err := run([]string{"-version", version, "-commit", commit, "-man", manDir(t), "archives", dist}, &bytes.Buffer{})
			wantErr(t, err, tt.want)
		})
	}
}

// An archive of a platform that the release does not build fails.
func TestArchivesOfOtherPlatforms(t *testing.T) {
	dist := t.TempDir()
	for _, p := range append(platforms, "linux/386") {
		goos, goarch, _ := strings.Cut(p, "/")
		writeArchive(t, filepath.Join(dist, archiveFile(goos, goarch)), goodArchive(goos, goarch))
	}
	err := run([]string{"-version", version, "-commit", commit, "-man", manDir(t), "archives", dist}, &bytes.Buffer{})
	wantErr(t, err, archiveFile("linux", "386")+": not an archive of a release platform")
}

// The image's binaries carry the version and the commit and are of the platform; its man pages are those of the
// release (RFC0006 requirements 10 and 13).
func TestImage(t *testing.T) {
	good := func(goarch string) map[string][]byte {
		files := map[string][]byte{"etc/passwd": []byte("nonroot")}
		for _, b := range imageBinaries {
			files["usr/local/bin/"+b] = fakeBinary("linux", goarch, version+" "+commit)
		}
		for _, p := range pages {
			files["usr/share/man/man1/"+p] = []byte(".TH")
		}
		return files
	}
	for _, tt := range []struct {
		name   string
		goarch string
		change func(map[string][]byte)
		want   string
	}{
		{"amd64", "amd64", func(map[string][]byte) {}, ""},
		{"arm64", "arm64", func(map[string][]byte) {}, ""},
		{"no man pages", "arm64", func(f map[string][]byte) {
			for _, p := range pages {
				delete(f, "usr/share/man/man1/"+p)
			}
		}, "/usr/share/man/man1 holds []"},
		{"no gateway agent", "amd64", func(f map[string][]byte) { delete(f, "usr/local/bin/strait-gateway") }, "lacks /usr/local/bin/strait-gateway"},
		{"a controller of another platform", "arm64", func(f map[string][]byte) {
			f["usr/local/bin/strait-controller"] = fakeBinary("linux", "amd64", version+" "+commit)
		}, "strait-controller: built for linux/amd64, want linux/arm64"},
		{"a binary without the version", "amd64", func(f map[string][]byte) {
			f["usr/local/bin/strait-gateway"] = fakeBinary("linux", "amd64", "dev "+commit)
		}, "strait-gateway: does not carry version"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			files := good(tt.goarch)
			tt.change(files)
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			writeFiles(t, tw, files)
			file := filepath.Join(t.TempDir(), "image.tar")
			if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			err := run([]string{"-version", version, "-commit", commit, "-man", manDir(t), "image", tt.goarch, file}, &bytes.Buffer{})
			wantErr(t, err, tt.want)
		})
	}
}

// wantErr fails unless err is nil when want is empty, or holds want.
func wantErr(t *testing.T, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Fatalf("want no error, have %v", err)
	case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
		t.Fatalf("want an error with %q, have %v", want, err)
	}
}
