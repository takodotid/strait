package version

import (
	"runtime/debug"
	"testing"
)

func TestResolve(t *testing.T) {
	vcs := func(revision, modified string) []debug.BuildSetting {
		return []debug.BuildSetting{{Key: "vcs", Value: "git"}, {Key: "vcs.revision", Value: revision}, {Key: "vcs.modified", Value: modified}}
	}
	for _, c := range []struct {
		name                    string
		version, commit         string
		info                    *debug.BuildInfo
		wantVersion, wantCommit string
	}{
		{"set at build time", "v1.0.0", "e76a8cd", &debug.BuildInfo{Main: debug.Module{Version: "v0.9.0"}, Settings: vcs("0000000", "true")}, "v1.0.0", "e76a8cd"},
		{"go install at a version", "dev", "unknown", &debug.BuildInfo{Main: debug.Module{Version: "v1.0.0"}}, "v1.0.0", "unknown"},
		{"go build in a checkout", "dev", "unknown", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: vcs("e76a8cd", "false")}, "dev", "e76a8cd"},
		{"go build in a changed checkout", "dev", "unknown", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: vcs("e76a8cd", "true")}, "dev", "e76a8cd-dirty"},
		{"no module version", "dev", "unknown", &debug.BuildInfo{}, "dev", "unknown"},
		{"no build information", "dev", "unknown", nil, "dev", "unknown"},
	} {
		v, commit := resolve(c.version, c.commit, c.info)
		if v != c.wantVersion || commit != c.wantCommit {
			t.Errorf("%s: got %q %q, want %q %q", c.name, v, commit, c.wantVersion, c.wantCommit)
		}
	}
}
