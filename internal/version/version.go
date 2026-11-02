// Package version holds the build's version and commit.
package version

import "runtime/debug"

// Version and Commit are set at build time with -ldflags -X. A value left unset is taken from the Go build information.
var (
	Version = "dev"
	Commit  = "unknown"
)

func init() {
	info, _ := debug.ReadBuildInfo()
	Version, Commit = resolve(Version, Commit, info)
}

// resolve returns the version and the commit. A version still dev takes the module's version, as go install sets it.
// A commit still unknown takes the VCS revision, as go build sets it in a checkout.
func resolve(version, commit string, info *debug.BuildInfo) (string, string) {
	if info == nil {
		return version, commit
	}
	if v := info.Main.Version; version == "dev" && v != "" && v != "(devel)" {
		version = v
	}
	if commit != "unknown" {
		return version, commit
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if revision == "" {
		return version, commit
	}
	if modified == "true" {
		revision += "-dirty"
	}
	return version, revision
}
