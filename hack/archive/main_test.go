package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// epoch is SOURCE_DATE_EPOCH in the tests: 2026-11-30T20:00:00Z, a day later in a zone 4 hours or more east of UTC.
const epoch = "1796068800"

var mtime = time.Unix(1796068800, 0).UTC()

const top = "strait_v1.3.1_linux_amd64"

// files are the files of an archive's directory. man.txt sorts before man/ by name, and after it in a walk of the
// directory.
var files = map[string]string{
	"strait":              "binary",
	"kubectl-strait":      "binary",
	"LICENSE":             "Apache",
	"SKILL.md":            "---\nname: strait\n---\n",
	"man.txt":             "not a directory",
	"man/man1/strait.1":   ".TH strait\n",
	"man/man1/strait-x.1": ".TH strait-x\n",
}

// executable reports the files that the build makes executable.
func executable(name string) bool { return name == "strait" || name == "kubectl-strait" }

// order is the order of the entries in every archive of files: by name.
var order = []string{
	top + "/",
	top + "/LICENSE",
	top + "/SKILL.md",
	top + "/kubectl-strait",
	top + "/man.txt",
	top + "/man/",
	top + "/man/man1/",
	top + "/man/man1/strait-x.1",
	top + "/man/man1/strait.1",
	top + "/strait",
}

// tree is how a build leaves the files on disk.
type tree struct {
	// umask clears these bits of every file and directory, as the build's umask does.
	umask os.FileMode
	// mtime is the time of every file and directory.
	mtime time.Time
	// reverse creates the files in reverse order of name.
	reverse bool
	// owner, when not zero, owns every file and directory. Only root can set it.
	owner int
}

// write writes the files into parent/top and returns that directory.
func (tr tree) write(t *testing.T, parent string) string {
	t.Helper()
	dir := filepath.Join(parent, top)
	names := slices.Sorted(maps.Keys(files))
	if tr.reverse {
		slices.Reverse(names)
	}
	for _, name := range names {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755&^tr.umask); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if executable(name) {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(files[name]), mode); err != nil {
			t.Fatal(err)
		}
	}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		perm := os.FileMode(0o644)
		if info.IsDir() || info.Mode()&0o100 != 0 {
			perm = 0o755
		}
		if err := os.Chmod(p, perm&^tr.umask); err != nil {
			return err
		}
		if tr.owner != 0 {
			if err := os.Lchown(p, tr.owner, tr.owner); err != nil {
				return err
			}
		}
		return os.Chtimes(p, tr.mtime, tr.mtime)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// inZone sets the local time zone for the rest of the test.
func inZone(t *testing.T, name string, offset int) {
	t.Helper()
	local := time.Local
	time.Local = time.FixedZone(name, offset)
	t.Cleanup(func() { time.Local = local })
}

// read is an entry as an archive holds it.
type read struct {
	name    string
	mode    int64
	content string
}

// readTarGz returns the entries of a .tar.gz in their order, and fails the test on a gzip header with a name or a time,
// or an entry with another time or an owner.
func readTarGz(t *testing.T, archive string) []read {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	if gz.Name != "" || !gz.ModTime.IsZero() {
		t.Errorf("gzip header: name %q, time %v, want neither", gz.Name, gz.ModTime)
	}
	var out []read
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if !h.ModTime.Equal(mtime) {
			t.Errorf("%s: time %v, want %v", h.Name, h.ModTime.UTC(), mtime)
		}
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Errorf("%s: owner %d:%d (%q:%q), want 0:0 with no names", h.Name, h.Uid, h.Gid, h.Uname, h.Gname)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, read{h.Name, h.Mode, string(b)})
	}
}

// readZip returns the entries of a .zip in their order, and fails the test on an entry with another time, in UTC or in
// its MS-DOS fields, or a file that is not deflated.
func readZip(t *testing.T, archive string) []read {
	t.Helper()
	r, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var out []read
	for _, f := range r.File {
		if !f.Modified.Equal(mtime) {
			t.Errorf("%s: time %v, want %v", f.Name, f.Modified.UTC(), mtime)
		}
		// 2026-11-30 20:00:00 in MS-DOS fields.
		if f.ModifiedDate != (2026-1980)<<9|11<<5|30 || f.ModifiedTime != 20<<11 {
			t.Errorf("%s: MS-DOS date %#x time %#x, want 2026-11-30 20:00:00, in UTC", f.Name, f.ModifiedDate, f.ModifiedTime)
		}
		if !f.FileInfo().IsDir() && f.Method != zip.Deflate {
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
		out = append(out, read{f.Name, int64(f.Mode().Perm()), string(b)})
	}
	return out
}

// The archive holds the directory and everything under it, sorted by name, with the content of each file. Each entry
// has the time SOURCE_DATE_EPOCH, and the mode 0755 for a directory or an executable, 0644 otherwise, whatever the
// umask left. A .tar.gz has no owner and an empty gzip header, a .zip its times in UTC (RFC0006 requirement 17).
func TestWrite(t *testing.T) {
	inZone(t, "UTC+7", 7*3600)
	for _, ext := range []string{".tar.gz", ".zip"} {
		t.Run(ext, func(t *testing.T) {
			tmp := t.TempDir()
			dir := tree{umask: 0o077, mtime: time.Now()}.write(t, tmp)
			archive := filepath.Join(tmp, "out"+ext)
			if err := run([]string{archive, dir + string(filepath.Separator)}, epoch); err != nil {
				t.Fatal(err)
			}
			var entries []read
			if ext == ".zip" {
				entries = readZip(t, archive)
			} else {
				entries = readTarGz(t, archive)
			}
			var names []string
			for _, e := range entries {
				names = append(names, e.name)
				name, isDir := e.name[len(top)+1:], e.name[len(e.name)-1] == '/'
				want := int64(0o644)
				if isDir || executable(name) {
					want = 0o755
				}
				if e.mode != want {
					t.Errorf("%s: mode %o, want %o", e.name, e.mode, want)
				}
				if !isDir && e.content != files[name] {
					t.Errorf("%s: content %q, want %q", e.name, e.content, files[name])
				}
			}
			if !slices.Equal(names, order) {
				t.Errorf("entries:\n got %q\nwant %q", names, order)
			}
		})
	}
}

// Two builds of the same files give the same bytes, whatever the files' times, the umask, the order in which the files
// were written, the time zone and, when the test runs as root, the files' owner (RFC0006 requirement 17).
func TestReproducible(t *testing.T) {
	other := tree{umask: 0o077, mtime: time.Now(), reverse: true}
	if os.Geteuid() == 0 {
		other.owner = 65534
	} else {
		t.Log("not root: both trees belong to the test's user, and TestWrite checks that the archive names no owner")
	}
	for _, ext := range []string{".tar.gz", ".zip"} {
		t.Run(ext, func(t *testing.T) {
			tmp := t.TempDir()
			a := tree{umask: 0o022, mtime: time.Unix(1700000000, 0)}.write(t, filepath.Join(tmp, "a"))
			b := other.write(t, filepath.Join(tmp, "b"))
			inZone(t, "UTC", 0)
			if err := run([]string{filepath.Join(tmp, "a"+ext), a}, epoch); err != nil {
				t.Fatal(err)
			}
			inZone(t, "UTC+14", 14*3600)
			if err := run([]string{filepath.Join(tmp, "b"+ext), b}, epoch); err != nil {
				t.Fatal(err)
			}
			x, err := os.ReadFile(filepath.Join(tmp, "a"+ext))
			if err != nil {
				t.Fatal(err)
			}
			y, err := os.ReadFile(filepath.Join(tmp, "b"+ext))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(x, y) {
				t.Errorf("two archives of the same files differ: %d and %d bytes", len(x), len(y))
			}
		})
	}
}

// A failed write leaves no archive behind.
func TestWriteFails(t *testing.T) {
	tmp := t.TempDir()
	dir := tree{mtime: time.Now()}.write(t, tmp)
	link := tree{mtime: time.Now()}.write(t, filepath.Join(tmp, "link"))
	if err := os.Symlink("strait", filepath.Join(link, "kubectl-strait.link")); err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		args  []string
		epoch string
	}{
		"missing directory": {[]string{filepath.Join(tmp, "out.zip"), filepath.Join(tmp, "missing")}, epoch},
		"usage":             {[]string{filepath.Join(tmp, "out.zip")}, epoch},
		"no epoch":          {[]string{filepath.Join(tmp, "out.tar.gz"), dir}, ""},
		"bad epoch":         {[]string{filepath.Join(tmp, "out.tar.gz"), dir}, "yesterday"},
		"negative epoch":    {[]string{filepath.Join(tmp, "out.tar.gz"), dir}, "-1"},
		"a symlink":         {[]string{filepath.Join(tmp, "out.tar.gz"), link}, epoch},
	} {
		if err := run(tt.args, tt.epoch); err == nil {
			t.Errorf("%s: no error", name)
		}
		for _, f := range []string{"out.zip", "out.tar.gz"} {
			if _, err := os.Stat(filepath.Join(tmp, f)); !os.IsNotExist(err) {
				t.Errorf("%s: %s left behind: %v", name, f, err)
			}
		}
	}
}
