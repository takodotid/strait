package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	version = "v1.3.0"
	commit  = "0123456789abcdef0123456789abcdef01234567"
	// epoch is SOURCE_DATE_EPOCH: 2026-11-30T20:00:00Z.
	epoch = "1796068800"
)

var mtime = time.Unix(1796068800, 0).UTC()

var pages = []string{"strait.1", "strait-status.1", "strait-version.1"}

// fakeBinary is a binary as the fakes of readBuildSettings and runVersion read it: its platform, what it carries, and
// its other build settings, -trimpath=true unless settings are given.
func fakeBinary(goos, goarch, carries string, settings ...string) []byte {
	if len(settings) == 0 {
		settings = []string{"-trimpath=true"}
	}
	return []byte(fmt.Sprintf("GOOS=%s GOARCH=%s %s %s", goos, goarch, carries, strings.Join(settings, " ")))
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

// entry is an entry of an archive that a test writes.
type entry struct {
	tar.Header
	data []byte
}

// entriesOf returns the entries that make dist writes for files: one for each file and each directory, sorted by
// name, each with the time SOURCE_DATE_EPOCH, owner 0, and the mode 0755 for a directory or a binary, 0644 otherwise.
func entriesOf(files map[string][]byte) []entry {
	all := map[string]entry{}
	for name, data := range files {
		mode := int64(0o644)
		if b := filepath.Base(name); b == "strait" || b == "kubectl-strait" || strings.HasSuffix(b, ".exe") {
			mode = 0o755
		}
		all[name] = entry{tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, ModTime: mtime, Size: int64(len(data))}, data}
		for d := filepath.Dir(name); d != "."; d = filepath.Dir(d) {
			all[d+"/"] = entry{Header: tar.Header{Name: d + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: mtime}}
		}
	}
	var out []entry
	for _, name := range slices.Sorted(maps.Keys(all)) {
		out = append(out, all[name])
	}
	return out
}

// writeArchive writes files into a .tar.gz or a .zip as make dist does.
func writeArchive(t *testing.T, file string, files map[string][]byte) {
	t.Helper()
	writeEntries(t, file, entriesOf(files), gzip.Header{})
}

// writeEntries writes the entries, in their order, into a .tar.gz with the gzip header gh, or into a .zip with each
// entry's time in its own zone, and its mode.
func writeEntries(t *testing.T, file string, entries []entry, gh gzip.Header) {
	t.Helper()
	var buf bytes.Buffer
	if strings.HasSuffix(file, ".zip") {
		zw := zip.NewWriter(&buf)
		for _, e := range entries {
			h := &zip.FileHeader{Name: e.Name, Modified: e.ModTime, Method: zip.Deflate}
			mode := os.FileMode(e.Mode)
			switch e.Typeflag {
			case tar.TypeDir:
				mode |= os.ModeDir
			case tar.TypeSymlink:
				mode |= os.ModeSymlink
			}
			h.SetMode(mode)
			w, err := zw.CreateHeader(h)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(e.data); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		gz := gzip.NewWriter(&buf)
		gz.Header = gh
		tw := tar.NewWriter(gz)
		for _, e := range entries {
			h := e.Header
			if err := tw.WriteHeader(&h); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(e.data); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
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
			t.Setenv("SOURCE_DATE_EPOCH", epoch)
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
	t.Setenv("SOURCE_DATE_EPOCH", epoch)
	dist := t.TempDir()
	for _, p := range append(platforms, "linux/386") {
		goos, goarch, _ := strings.Cut(p, "/")
		writeArchive(t, filepath.Join(dist, archiveFile(goos, goarch)), goodArchive(goos, goarch))
	}
	err := run([]string{"-version", version, "-commit", commit, "-man", manDir(t), "archives", dist}, &bytes.Buffer{})
	wantErr(t, err, archiveFile("linux", "386")+": not an archive of a release platform")
}

// writeDist writes the six archives as make dist does, after change edits the entries of the platform p.
func writeDist(t *testing.T, p string, change func([]entry) []entry, gh gzip.Header) string {
	t.Helper()
	dist := t.TempDir()
	for _, q := range platforms {
		goos, goarch, _ := strings.Cut(q, "/")
		entries, h := entriesOf(goodArchive(goos, goarch)), gzip.Header{}
		if q == p {
			entries, h = change(entries), gh
		}
		writeEntries(t, filepath.Join(dist, archiveFile(goos, goarch)), entries, h)
	}
	return dist
}

// set changes the entry named name with f.
func set(name string, f func(*entry)) func([]entry) []entry {
	return func(entries []entry) []entry {
		for i := range entries {
			if entries[i].Name == name {
				f(&entries[i])
			}
		}
		return entries
	}
}

// An archive passes only when each entry is what make dist writes from SOURCE_DATE_EPOCH, and the error names the
// entry that is not (RFC0006 requirement 17).
func TestArchiveEntries(t *testing.T) {
	dir := "strait_" + version + "_linux_amd64/"
	win := "strait_" + version + "_windows_arm64/"
	same := func(e []entry) []entry { return e }
	for _, tt := range []struct {
		name   string
		p      string
		change func([]entry) []entry
		gzip   gzip.Header
		want   string
	}{
		{"good", "linux/amd64", same, gzip.Header{}, ""},
		{"the build's time", "linux/amd64", set(dir+"LICENSE", func(e *entry) { e.ModTime = time.Unix(1793000000, 0) }),
			gzip.Header{}, "entry " + dir + "LICENSE: time 2026-10-26T"},
		{"an access time", "linux/amd64", set(dir+"README.md", func(e *entry) { e.AccessTime, e.Format = mtime, tar.FormatPAX }),
			gzip.Header{}, "entry " + dir + "README.md: an access or a change time, want neither"},
		{"the build's user", "linux/amd64", set(dir+"strait", func(e *entry) { e.Uid, e.Gid, e.Uname, e.Gname = 1001, 1001, "runner", "runner" }),
			gzip.Header{}, `entry ` + dir + `strait: owner 1001:1001 ("runner":"runner"), want 0:0`},
		{"an owner's name", "linux/amd64", set(dir+"man/", func(e *entry) { e.Uname = "root" }),
			gzip.Header{}, "entry " + dir + "man/: owner 0:0"},
		{"the umask's mode", "linux/amd64", set(dir+"SKILL.md", func(e *entry) { e.Mode = 0o664 }),
			gzip.Header{}, "entry " + dir + "SKILL.md: mode 664, want 644"},
		{"a binary that does not run", "linux/amd64", set(dir+"kubectl-strait", func(e *entry) { e.Mode = 0o644 }),
			gzip.Header{}, "entry " + dir + "kubectl-strait: mode 644, want 755"},
		{"a document that runs", "linux/amd64", set(dir+"LICENSE", func(e *entry) { e.Mode = 0o755 }),
			gzip.Header{}, "entry " + dir + "LICENSE: mode 755, want 644"},
		{"the file system's order", "linux/amd64", func(e []entry) []entry { slices.Reverse(e); return e },
			gzip.Header{}, "comes after " + dir + "strait, want the entries sorted by name"},
		{"a symlink", "linux/amd64", func(e []entry) []entry {
			return append(e, entry{Header: tar.Header{Name: dir + "w", Typeflag: tar.TypeSymlink, Linkname: "strait", Mode: 0o644, ModTime: mtime}})
		}, gzip.Header{}, "entry " + dir + "w: type '2', want a directory or a regular file"},
		{"a directory without a slash", "linux/amd64", set(dir+"man/", func(e *entry) { e.Name = dir + "man" }),
			gzip.Header{}, "entry " + dir + "man: a directory's name must end with a slash"},
		{"the gzip header's time", "linux/amd64", same, gzip.Header{ModTime: time.Unix(1793000000, 0)},
			archiveFile("linux", "amd64") + ": the gzip header holds the name \"\" and the time 2026-10-26T"},
		{"the gzip header's name", "linux/amd64", same, gzip.Header{Name: "strait.tar"},
			"the gzip header holds the name \"strait.tar\""},
		{"a zip time of the build", "windows/arm64", set(win+"strait.exe", func(e *entry) { e.ModTime = time.Unix(1793000000, 0) }),
			gzip.Header{}, "entry " + win + "strait.exe: time 2026-10-26T"},
		{"a zip time in a local zone", "windows/arm64", set(win+"README.md", func(e *entry) { e.ModTime = mtime.In(time.FixedZone("UTC+7", 7*3600)) }),
			gzip.Header{}, "entry " + win + "README.md: time 2026-11-30T20:00:00Z, MS-DOS 2026-12-01 03:00:00, want SOURCE_DATE_EPOCH 2026-11-30T20:00:00Z, in UTC in both"},
		{"a zip directory of the umask", "windows/arm64", set(win+"man/man1/", func(e *entry) { e.Mode = 0o700 }),
			gzip.Header{}, "entry " + win + "man/man1/: mode 700, want 755"},
		{"a zip binary that does not run", "windows/arm64", set(win+"strait.exe", func(e *entry) { e.Mode = 0o644 }),
			gzip.Header{}, "entry " + win + "strait.exe: mode 644, want 755"},
		{"a zip symlink", "windows/arm64", func(e []entry) []entry {
			return append(e, entry{Header: tar.Header{Name: win + "w", Typeflag: tar.TypeSymlink, Mode: 0o777, ModTime: mtime}, data: []byte("strait.exe")})
		}, gzip.Header{}, "entry " + win + "w: type L"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SOURCE_DATE_EPOCH", epoch)
			dist := writeDist(t, tt.p, tt.change, tt.gzip)
			err := run([]string{"-version", version, "-commit", commit, "-man", manDir(t), "archives", dist}, &bytes.Buffer{})
			wantErr(t, err, tt.want)
		})
	}
}

// The archives are checked against SOURCE_DATE_EPOCH, which must be set. The image is not.
func TestArchivesNeedEpoch(t *testing.T) {
	dist := writeDist(t, "", nil, gzip.Header{})
	for _, v := range []string{"", "yesterday", "-1"} {
		t.Setenv("SOURCE_DATE_EPOCH", v)
		err := run([]string{"-version", version, "-commit", commit, "-man", manDir(t), "archives", dist}, &bytes.Buffer{})
		wantErr(t, err, fmt.Sprintf("SOURCE_DATE_EPOCH %q is not a number of seconds", v))
	}
	t.Setenv("SOURCE_DATE_EPOCH", "1793000000")
	err := run([]string{"-version", version, "-commit", commit, "-man", manDir(t), "archives", dist}, &bytes.Buffer{})
	wantErr(t, err, "want SOURCE_DATE_EPOCH 2026-10-26T")
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
		{"a binary built without -trimpath", "amd64", func(f map[string][]byte) {
			f["usr/local/bin/strait-gateway"] = fakeBinary("linux", "amd64", version+" "+commit, "-trimpath=false")
		}, "strait-gateway: built without -trimpath"},
		{"a binary stamped with the commit, not modified", "amd64", func(f map[string][]byte) {
			f["usr/local/bin/strait"] = fakeBinary("linux", "amd64", version+" "+commit, "-trimpath=true", "vcs.revision="+commit, "vcs.modified=false")
		}, ""},
		{"a binary stamped with another commit", "amd64", func(f map[string][]byte) {
			f["usr/local/bin/strait"] = fakeBinary("linux", "amd64", version+" "+commit, "-trimpath=true", "vcs.revision=0123abcd", "vcs.modified=false")
		}, "strait: stamped with commit 0123abcd, modified false; want commit " + commit},
		{"a binary stamped as modified", "amd64", func(f map[string][]byte) {
			f["usr/local/bin/strait"] = fakeBinary("linux", "amd64", version+" "+commit, "-trimpath=true", "vcs.revision="+commit, "vcs.modified=true")
		}, "strait: stamped with commit " + commit + ", modified true"},
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

// The index of v1.4.0, as published, holds BuildKit's attestation manifests. The index of a build of ade11e2 with the
// release's settings holds the platform images only.
func TestIndex(t *testing.T) {
	const (
		v140, c140 = "v1.4.0", "8dea4ffa3c5339e583e0d3e5a44593fb8a5e1429"
		v150, c150 = "v1.5.0", "ade11e2367ea8ff76e89830e38830adfb6fbdf87"
	)
	good, err := os.ReadFile("testdata/index-ade11e2.json")
	if err != nil {
		t.Fatal(err)
	}
	// changed writes the good index with a change, and returns its file and its digest.
	changed := func(t *testing.T, change func(map[string]any)) (string, string) {
		var idx map[string]any
		if err := json.Unmarshal(good, &idx); err != nil {
			t.Fatal(err)
		}
		change(idx)
		b, err := json.Marshal(idx)
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(t.TempDir(), "index.json")
		if err := os.WriteFile(file, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return file, fmt.Sprintf("sha256:%x", sha256.Sum256(b))
	}
	manifests := func(idx map[string]any) []any { return idx["manifests"].([]any) }
	for _, tt := range []struct {
		name            string
		version, commit string
		change          func(map[string]any)
		file, digest    string
		want            string
	}{
		{name: "ade11e2", version: v150, commit: c150, file: "testdata/index-ade11e2.json",
			digest: "sha256:397740dc8bfd114ee8d03e9b0eb5a24641bb9021f05c4ea72f9e9c5727dd0a78"},
		{name: "v1.4.0", version: v140, commit: c140, file: "testdata/index-v1.4.0.json",
			digest: "sha256:2f779679cb81a6a3f66ae5c2d12f4be06edfd683139b957bea7a82caeb2c5afd",
			want:   "platform unknown/unknown, an attestation-manifest"},
		{name: "another digest", version: v150, commit: c150, file: "testdata/index-ade11e2.json",
			digest: "sha256:2f779679cb81a6a3f66ae5c2d12f4be06edfd683139b957bea7a82caeb2c5afd",
			want:   "hashes to sha256:397740dc"},
		{name: "unchanged", version: v150, commit: c150, change: func(map[string]any) {}},
		{name: "another version", version: "v1.5.1", commit: c150, change: func(map[string]any) {},
			want: `annotation org.opencontainers.image.version is "1.5.0", want "1.5.1"`},
		{name: "another commit", version: v150, commit: c140, change: func(map[string]any) {},
			want: `annotation org.opencontainers.image.revision is "` + c150},
		{name: "no arm64", version: v150, commit: c150, change: func(idx map[string]any) {
			idx["manifests"] = manifests(idx)[:1]
		}, want: "lists the platforms [linux/amd64], want [linux/amd64 linux/arm64]"},
		{name: "a third platform", version: v150, commit: c150, change: func(idx map[string]any) {
			idx["manifests"] = append(manifests(idx), map[string]any{
				"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": "sha256:00",
				"platform": map[string]any{"os": "linux", "architecture": "arm", "variant": "v7"},
			})
		}, want: "lists the platforms [linux/amd64 linux/arm/v7 linux/arm64]"},
		{name: "a Docker manifest", version: v150, commit: c150, change: func(idx map[string]any) {
			manifests(idx)[1].(map[string]any)["mediaType"] = "application/vnd.docker.distribution.manifest.v2+json"
		}, want: "linux/arm64 is \"application/vnd.docker.distribution.manifest.v2+json\""},
		{name: "a Docker manifest list", version: v150, commit: c150, change: func(idx map[string]any) {
			idx["mediaType"] = "application/vnd.docker.distribution.manifest.list.v2+json"
		}, want: "index: media type"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			file, digest := tt.file, tt.digest
			if tt.change != nil {
				file, digest = changed(t, tt.change)
			}
			// The index needs no man pages.
			err := run([]string{"-version", tt.version, "-commit", tt.commit, "index", digest, file}, &bytes.Buffer{})
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
