// Command distcheck checks the CLI archives of a release against RFC0006 requirements 10, 13, 15, 16 and 17, the files
// of one platform of its image against requirements 10 and 13, or the image's index against RFC0007 requirement 30.
// The release workflow runs it before it publishes anything.
//
//	SOURCE_DATE_EPOCH=<seconds> go run ./hack/distcheck -version vX.Y.Z -commit SHA -man DIR archives DIST
//	go run ./hack/distcheck -version vX.Y.Z -commit SHA -man DIR image ARCH FILE.tar
//	go run ./hack/distcheck -version vX.Y.Z -commit SHA index DIGEST FILE.json
//
// DIR holds the man pages that hack/gendocs writes. DIST holds the archives that make dist writes, with
// SOURCE_DATE_EPOCH as the time of every entry. FILE.tar is the image's file system, as docker export writes it.
// FILE.json is the image's index, as the registry serves it for DIGEST.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// platforms are the platforms of the CLI archives (RFC0006 requirement 15).
var platforms = []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64"}

// imageBinaries are the binaries of the image, in /usr/local/bin.
var imageBinaries = []string{"strait", "strait-controller", "strait-gateway"}

// imagePlatforms are the platforms of the image: its index lists their images and nothing else.
var imagePlatforms = []string{"linux/amd64", "linux/arm64"}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "distcheck:", err)
		os.Exit(1)
	}
}

// check is what every binary must carry.
type check struct {
	version, commit string
	// man is the name of each man page, as hack/gendocs writes them.
	man []string
	// tmp holds the binaries that are read or run.
	tmp string
	// epoch is SOURCE_DATE_EPOCH, the time of every entry of the archives.
	epoch time.Time
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("distcheck", flag.ContinueOnError)
	version := fs.String("version", "", "The release's version, vX.Y.Z.")
	commit := fs.String("commit", "", "The commit the release is built from.")
	man := fs.String("man", "", "The directory of the man pages that hack/gendocs writes.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *version == "" || *commit == "" {
		return errors.New("-version and -commit are required")
	}
	if rest := fs.Args(); len(rest) == 3 && rest[0] == "index" {
		c := check{version: *version, commit: *commit}
		if err := errors.Join(c.index(rest[1], rest[2])...); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "distcheck: %s %s: every check passed\n", strings.Join(rest, " "), *version)
		return nil
	}
	if *man == "" {
		return errors.New("-man is required for archives and image")
	}
	pages, err := os.ReadDir(*man)
	if err != nil {
		return err
	}
	c := check{version: *version, commit: *commit}
	for _, p := range pages {
		c.man = append(c.man, p.Name())
	}
	if len(c.man) == 0 {
		return fmt.Errorf("no man page in %s", *man)
	}
	tmp, err := os.MkdirTemp("", "distcheck-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	c.tmp = tmp

	var errs []error
	switch rest := fs.Args(); {
	case len(rest) == 2 && rest[0] == "archives":
		epoch := os.Getenv("SOURCE_DATE_EPOCH")
		secs, err := strconv.ParseInt(epoch, 10, 64)
		if err != nil || secs < 0 {
			return fmt.Errorf("SOURCE_DATE_EPOCH %q is not a number of seconds: it is the time of every entry of the archives", epoch)
		}
		c.epoch = time.Unix(secs, 0).UTC()
		errs = c.archives(rest[1])
	case len(rest) == 3 && rest[0] == "image":
		errs = c.image(rest[1], rest[2])
	default:
		return errors.New("usage: distcheck -version V -commit C [-man DIR] archives DIST | image ARCH FILE.tar | index DIGEST FILE.json")
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "distcheck: %s %s: every check passed\n", strings.Join(fs.Args(), " "), *version)
	return nil
}

// archiveName is the name of a platform's archive, with no extension.
func (c check) archiveName(goos, goarch string) string {
	return fmt.Sprintf("strait_%s_%s_%s", c.version, goos, goarch)
}

// archives checks that dist holds one archive per platform and no other of this version (requirement 15), each with
// one directory named after it that holds exactly the files of requirement 16, the man pages of requirement 10, and
// binaries of its platform that carry the version and the commit (requirement 13). Each entry of an archive must be
// what make dist writes from SOURCE_DATE_EPOCH (requirement 17).
func (c check) archives(dist string) []error {
	var errs []error
	want := map[string]string{}
	for _, p := range platforms {
		goos, goarch, _ := strings.Cut(p, "/")
		name := c.archiveName(goos, goarch)
		ext := ".tar.gz"
		if goos == "windows" {
			ext = ".zip"
		}
		want[name+ext] = p
	}
	found, err := filepath.Glob(filepath.Join(dist, "strait_"+c.version+"_*"))
	if err != nil {
		return []error{err}
	}
	for _, f := range found {
		// make dist leaves each archive's directory next to it.
		if st, err := os.Stat(f); err == nil && st.IsDir() {
			continue
		}
		if _, ok := want[filepath.Base(f)]; !ok {
			errs = append(errs, fmt.Errorf("%s: not an archive of a release platform", filepath.Base(f)))
		}
	}
	for _, file := range slices.Sorted(maps.Keys(want)) {
		goos, goarch, _ := strings.Cut(want[file], "/")
		files, err := readArchive(filepath.Join(dist, file))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", file, err))
			continue
		}
		for _, e := range c.archive(c.archiveName(goos, goarch), goos, goarch, files) {
			errs = append(errs, fmt.Errorf("%s: %w", file, e))
		}
		for _, e := range c.entries(filepath.Join(dist, file), binaries(c.archiveName(goos, goarch), goos)) {
			errs = append(errs, fmt.Errorf("%s: %w", file, e))
		}
	}
	return errs
}

// binaries returns the paths of the binaries in the archive of dir.
func binaries(dir, goos string) []string {
	exe := ""
	if goos == "windows" {
		exe = ".exe"
	}
	return []string{dir + "/strait" + exe, dir + "/kubectl-strait" + exe}
}

// archive checks the files of one archive, by path.
func (c check) archive(dir, goos, goarch string, files map[string][]byte) []error {
	bins := binaries(dir, goos)
	want := map[string]bool{}
	for _, f := range append([]string{dir + "/SKILL.md", dir + "/LICENSE", dir + "/README.md"}, bins...) {
		want[f] = true
	}
	for _, m := range c.man {
		want[dir+"/man/man1/"+m] = true
	}
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if !want[name] {
			errs = append(errs, fmt.Errorf("holds %s, which is not a file of %s/ (RFC0006 requirements 10 and 16)", name, dir))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if _, ok := files[name]; !ok {
			errs = append(errs, fmt.Errorf("lacks %s (RFC0006 requirements 10 and 16)", name))
		}
	}
	for _, b := range bins {
		if data, ok := files[b]; ok {
			if err := c.binary(path.Base(b), goos, goarch, data); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errs
}

// entries checks what make dist fixes in each entry of an archive, so that two builds of one commit give the same bytes
// (requirement 17): the entries sorted by name, each with the time SOURCE_DATE_EPOCH, and the mode 0755 for a directory
// or a binary, 0644 otherwise. A .tar.gz must have owner 0 with no names in each entry and no name or time in its gzip
// header. A .zip must store its times in UTC. Each error names the entry.
func (c check) entries(file string, binaries []string) []error {
	var errs []error
	fail := func(name, format string, args ...any) {
		errs = append(errs, fmt.Errorf("entry %s: %s (RFC0006 requirement 17)", name, fmt.Sprintf(format, args...)))
	}
	prev := ""
	// both checks what the two formats share: the order, the kind, and the mode of the kind.
	both := func(name string, dir bool, mode int64) {
		if prev != "" && name <= prev {
			fail(name, "comes after %s, want the entries sorted by name", prev)
		}
		prev = name
		if dir != strings.HasSuffix(name, "/") {
			fail(name, "a directory's name must end with a slash, and only a directory's")
		}
		want := int64(0o644)
		if dir || slices.Contains(binaries, name) {
			want = 0o755
		}
		if mode != want {
			fail(name, "mode %o, want %o", mode, want)
		}
	}

	if strings.HasSuffix(file, ".zip") {
		r, err := zip.OpenReader(file)
		if err != nil {
			return []error{err}
		}
		defer r.Close()
		date, clock := msDosTime(c.epoch)
		for _, f := range r.File {
			// The MS-DOS fields hold no zone, and readers take them as local time. They must hold the time in UTC.
			if !f.Modified.Equal(c.epoch) || f.ModifiedDate != date || f.ModifiedTime != clock {
				fail(f.Name, "time %s, MS-DOS %s, want SOURCE_DATE_EPOCH %s, in UTC in both",
					f.Modified.UTC().Format(time.RFC3339), msDosString(f.ModifiedDate, f.ModifiedTime), c.epoch.Format(time.RFC3339))
			}
			mode := f.Mode()
			if t := mode.Type(); t != 0 && t != fs.ModeDir {
				fail(f.Name, "type %s, want a directory or a regular file", t)
			}
			both(f.Name, mode.IsDir(), int64(mode.Perm()))
		}
		return errs
	}

	f, err := os.Open(file)
	if err != nil {
		return []error{err}
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return []error{err}
	}
	if gz.Name != "" || !gz.ModTime.IsZero() {
		errs = append(errs, fmt.Errorf("the gzip header holds the name %q and the time %s, want neither (RFC0006 requirement 17)",
			gz.Name, gz.ModTime.UTC().Format(time.RFC3339)))
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return errs
		}
		if err != nil {
			return append(errs, err)
		}
		if !h.ModTime.Equal(c.epoch) {
			fail(h.Name, "time %s, want SOURCE_DATE_EPOCH %s", h.ModTime.UTC().Format(time.RFC3339), c.epoch.Format(time.RFC3339))
		}
		if !h.AccessTime.IsZero() || !h.ChangeTime.IsZero() {
			fail(h.Name, "an access or a change time, want neither")
		}
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			fail(h.Name, "owner %d:%d (%q:%q), want 0:0 with no names", h.Uid, h.Gid, h.Uname, h.Gname)
		}
		if h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeReg {
			fail(h.Name, "type %q, want a directory or a regular file", h.Typeflag)
		}
		both(h.Name, h.Typeflag == tar.TypeDir, h.Mode)
	}
}

// msDosTime returns the MS-DOS date and time of t in UTC, as a .zip entry stores them, to 2 seconds.
func msDosTime(t time.Time) (date, clock uint16) {
	t = t.UTC()
	date = uint16(t.Day() + int(t.Month())<<5 + (t.Year()-1980)<<9)
	clock = uint16(t.Second()/2 + t.Minute()<<5 + t.Hour()<<11)
	return date, clock
}

// msDosString formats an MS-DOS date and time.
func msDosString(date, clock uint16) string {
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d",
		int(date>>9)+1980, date>>5&0xf, date&0x1f, clock>>11, clock>>5&0x3f, clock&0x1f*2)
}

// image checks one platform of the image, from the tar of its file system: the binaries of its platform, which carry
// the version and the commit (requirement 13), and the man pages (requirement 10).
func (c check) image(goarch, file string) []error {
	f, err := os.Open(file)
	if err != nil {
		return []error{err}
	}
	defer f.Close()
	files, err := readTar(f)
	if err != nil {
		return []error{fmt.Errorf("%s: %w", file, err)}
	}
	var errs []error
	for _, b := range imageBinaries {
		data, ok := files["usr/local/bin/"+b]
		if !ok {
			errs = append(errs, fmt.Errorf("image: lacks /usr/local/bin/%s", b))
			continue
		}
		if err := c.binary(b, "linux", goarch, data); err != nil {
			errs = append(errs, fmt.Errorf("image: %w", err))
		}
	}
	var pages []string
	for name := range files {
		if dir, page := path.Split(name); dir == "usr/share/man/man1/" {
			pages = append(pages, page)
		}
	}
	slices.Sort(pages)
	if want := slices.Sorted(slices.Values(c.man)); !slices.Equal(pages, want) {
		errs = append(errs, fmt.Errorf("image: /usr/share/man/man1 holds %v, want %v (RFC0006 requirement 10)", pages, want))
	}
	return errs
}

// index checks the image's index (RFC0007 requirement 30): FILE hashes to the digest, as the chart names it, and the
// index lists one OCI image manifest per platform of imagePlatforms and nothing else, so no attestation. Its
// annotations name the release's version and commit.
func (c check) index(digest, file string) []error {
	data, err := os.ReadFile(file)
	if err != nil {
		return []error{err}
	}
	if got := fmt.Sprintf("sha256:%x", sha256.Sum256(data)); got != digest {
		return []error{fmt.Errorf("index: %s hashes to %s, want %s", file, got, digest)}
	}
	var idx struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Platform  struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
				Variant      string `json:"variant"`
			} `json:"platform"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(data, &idx); err != nil {
		return []error{fmt.Errorf("index: %s: %w", file, err)}
	}
	var errs []error
	if want := "application/vnd.oci.image.index.v1+json"; idx.MediaType != want {
		errs = append(errs, fmt.Errorf("index: media type %q, want %q", idx.MediaType, want))
	}
	var listed []string
	for _, m := range idx.Manifests {
		p := path.Join(m.Platform.OS, m.Platform.Architecture, m.Platform.Variant)
		if t := m.Annotations["vnd.docker.reference.type"]; t != "" {
			errs = append(errs, fmt.Errorf("index: lists %s, platform %s, an %s: the provenance and the SBOM are attested apart from the index", m.Digest, p, t))
			continue
		}
		if want := "application/vnd.oci.image.manifest.v1+json"; m.MediaType != want {
			errs = append(errs, fmt.Errorf("index: %s is %q, want %q", p, m.MediaType, want))
		}
		listed = append(listed, p)
	}
	slices.Sort(listed)
	if !slices.Equal(listed, imagePlatforms) {
		errs = append(errs, fmt.Errorf("index: lists the platforms %v, want %v", listed, imagePlatforms))
	}
	for _, a := range [][2]string{
		{"org.opencontainers.image.version", strings.TrimPrefix(c.version, "v")},
		{"org.opencontainers.image.revision", c.commit},
	} {
		if got := idx.Annotations[a[0]]; got != a[1] {
			errs = append(errs, fmt.Errorf("index: annotation %s is %q, want %q", a[0], got, a[1]))
		}
	}
	return errs
}

// binary checks that a binary was built for its platform, and that it carries the version and the commit. The binary
// of the host's platform is also run: `strait version` must print them (requirement 13). The others are searched for
// both strings, which only the build's -X flags put there when it ran with no .git, as the image's build does. It
// must be built with -trimpath, and a VCS stamp, which a build in a git checkout adds, must name the commit with no
// change (requirement 21). The image's build has no .git, so its binaries carry no VCS stamp.
func (c check) binary(name, goos, goarch string, data []byte) error {
	p, err := os.CreateTemp(c.tmp, name+"-")
	if err != nil {
		return err
	}
	if _, err := p.Write(data); err != nil {
		p.Close()
		return err
	}
	if err := p.Close(); err != nil {
		return err
	}
	settings, err := readBuildSettings(p.Name())
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if settings["GOOS"] != goos || settings["GOARCH"] != goarch {
		return fmt.Errorf("%s: built for %s/%s, want %s/%s", name, settings["GOOS"], settings["GOARCH"], goos, goarch)
	}
	if settings["-trimpath"] != "true" {
		return fmt.Errorf("%s: built without -trimpath, so it holds the build machine's paths (RFC0006 requirement 21)", name)
	}
	if rev, ok := settings["vcs.revision"]; ok && (rev != c.commit || settings["vcs.modified"] != "false") {
		return fmt.Errorf("%s: stamped with commit %s, modified %s; want commit %s, not modified (RFC0006 requirement 21)",
			name, rev, settings["vcs.modified"], c.commit)
	}
	if !bytes.Contains(data, []byte(c.version)) || !bytes.Contains(data, []byte(c.commit)) {
		return fmt.Errorf("%s: does not carry version %s and commit %s (RFC0006 requirement 13)", name, c.version, c.commit)
	}
	if name != "strait" || goos != runtime.GOOS || goarch != runtime.GOARCH {
		return nil
	}
	if err := os.Chmod(p.Name(), 0o755); err != nil {
		return err
	}
	out, err := runVersion(p.Name())
	if err != nil {
		return fmt.Errorf("strait version: %w: %s", err, out)
	}
	for _, line := range []string{"Version:  " + c.version, "Commit:   " + c.commit} {
		if !slices.Contains(strings.Split(strings.TrimSpace(out), "\n"), line) {
			return fmt.Errorf("strait version prints %q, want a line %q (RFC0006 requirement 13)", out, line)
		}
	}
	return nil
}

// readBuildSettings returns the build settings that Go records in a binary, such as GOOS and GOARCH. Tests replace it.
var readBuildSettings = func(file string) (map[string]string, error) {
	info, err := buildinfo.ReadFile(file)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, s := range info.Settings {
		out[s.Key] = s.Value
	}
	return out, nil
}

// runVersion runs `strait version` with no kubeconfig. Tests replace it.
var runVersion = func(file string) (string, error) {
	cmd := exec.Command(file, "version")
	cmd.Env = []string{"HOME=" + filepath.Dir(file)}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// readArchive returns the regular files of a .tar.gz or a .zip, by path. A directory entry must end with a slash, and
// is left out.
func readArchive(file string) (map[string][]byte, error) {
	if strings.HasSuffix(file, ".zip") {
		r, err := zip.OpenReader(file)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		out := map[string][]byte{}
		for _, f := range r.File {
			if strings.HasSuffix(f.Name, "/") {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			data, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return nil, err
			}
			out[f.Name] = data
		}
		return out, nil
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	return readTar(gz)
}

// readTar returns the regular files of a tar, by path, with no leading ./ or /.
func readTar(r io.Reader) (map[string][]byte, error) {
	out := map[string][]byte{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[strings.TrimPrefix(strings.TrimPrefix(h.Name, "./"), "/")] = data
	}
}
