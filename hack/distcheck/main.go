// Command distcheck checks the CLI archives of a release, or the files of one platform of its image, against RFC0006
// requirements 10, 13, 15 and 16. The release workflow runs it before it publishes anything.
//
//	go run ./hack/distcheck -version vX.Y.Z -commit SHA -man DIR archives DIST
//	go run ./hack/distcheck -version vX.Y.Z -commit SHA -man DIR image ARCH FILE.tar
//
// DIR holds the man pages that hack/gendocs writes. DIST holds the archives that make dist writes. FILE.tar is the
// image's file system, as docker export writes it.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"debug/buildinfo"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// platforms are the platforms of the CLI archives (RFC0006 requirement 15).
var platforms = []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64"}

// imageBinaries are the binaries of the image, in /usr/local/bin.
var imageBinaries = []string{"strait", "strait-controller", "strait-gateway"}

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
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("distcheck", flag.ContinueOnError)
	version := fs.String("version", "", "The release's version, vX.Y.Z.")
	commit := fs.String("commit", "", "The commit the release is built from.")
	man := fs.String("man", "", "The directory of the man pages that hack/gendocs writes.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *version == "" || *commit == "" || *man == "" {
		return errors.New("-version, -commit and -man are required")
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
		errs = c.archives(rest[1])
	case len(rest) == 3 && rest[0] == "image":
		errs = c.image(rest[1], rest[2])
	default:
		return errors.New("usage: distcheck -version V -commit C -man DIR archives DIST | image ARCH FILE.tar")
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
// binaries of its platform that carry the version and the commit (requirement 13).
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
	}
	return errs
}

// archive checks the files of one archive, by path.
func (c check) archive(dir, goos, goarch string, files map[string][]byte) []error {
	exe := ""
	if goos == "windows" {
		exe = ".exe"
	}
	want := map[string]bool{}
	for _, f := range []string{"strait" + exe, "kubectl-strait" + exe, "SKILL.md", "LICENSE", "README.md"} {
		want[dir+"/"+f] = true
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
	for _, b := range []string{"strait" + exe, "kubectl-strait" + exe} {
		if data, ok := files[dir+"/"+b]; ok {
			if err := c.binary(b, goos, goarch, data); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errs
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

// binary checks that a binary was built for its platform, and that it carries the version and the commit. The binary
// of the host's platform is also run: `strait version` must print them (requirement 13). The others are searched for
// both strings, which only the build's -X flags put there when it ran with no .git, as the image's build does.
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
		return fmt.Errorf("%s: built for %s/%s, want %s/%s (RFC0006 requirement 15)", name, settings["GOOS"], settings["GOARCH"], goos, goarch)
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
