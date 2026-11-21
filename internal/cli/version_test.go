package cli

import (
	"encoding/json"
	"runtime"
	"runtime/debug"
	"testing"
)

func buildInfoWith(mainVersion string, settings map[string]string) *debug.BuildInfo {
	info := &debug.BuildInfo{Main: debug.Module{Version: mainVersion}}
	for k, v := range settings {
		info.Settings = append(info.Settings, debug.BuildSetting{Key: k, Value: v})
	}
	return info
}

func TestVersionPrintsBuildDetailsAsJSON(t *testing.T) {
	r := run("version")

	if r.code != ExitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %q)", r.code, ExitOK, r.stderr)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q, want empty", r.stderr)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("stdout is not a JSON object of strings: %v\nstdout: %q", err, r.stdout)
	}
	for _, key := range []string{"version", "commit", "date", "go", "os", "arch"} {
		if got[key] == "" {
			t.Errorf("%s is empty in %q", key, r.stdout)
		}
	}
	if len(got) != 6 {
		t.Errorf("keys = %v, want exactly version, commit, date, go, os, arch", got)
	}
	if got["go"] != runtime.Version() || got["os"] != runtime.GOOS || got["arch"] != runtime.GOARCH {
		t.Errorf("runtime fields = %q/%q/%q, want %s/%s/%s", got["go"], got["os"], got["arch"], runtime.Version(), runtime.GOOS, runtime.GOARCH)
	}
}

func TestVersionReportsTheLinkedValues(t *testing.T) {
	oldVersion, oldCommit, oldDate := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = oldVersion, oldCommit, oldDate })
	Version, Commit, Date = "1.2.3", "abc1234", "2026-11-21T10:00:00Z"

	r := run("version")

	var got map[string]string
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\nstdout: %q", err, r.stdout)
	}
	if got["version"] != "1.2.3" || got["commit"] != "abc1234" || got["date"] != "2026-11-21T10:00:00Z" {
		t.Errorf("version output = %v, want the linked values", got)
	}
}

func TestResolveBuild(t *testing.T) {
	embedded := buildInfoWith("v0.4.2", map[string]string{"vcs.revision": "deadbeef", "vcs.time": "2026-01-02T03:04:05Z"})
	cases := []struct {
		name         string
		version      string
		commit, date string
		info         *debug.BuildInfo
		want         buildDetails
	}{
		{
			name:    "linked values win and lose a leading v",
			version: "v1.0.0", commit: "c0ffee", date: "2026-02-03T00:00:00Z",
			info: embedded,
			want: buildDetails{Version: "1.0.0", Commit: "c0ffee", Date: "2026-02-03T00:00:00Z"},
		},
		{
			name: "build info carrying a module version and vcs settings",
			info: embedded,
			want: buildDetails{Version: "0.4.2", Commit: "deadbeef", Date: "2026-01-02T03:04:05Z"},
		},
		{
			name: "go install of a module version records the version but no vcs data",
			info: buildInfoWith("v0.0.0-20261121100000-0123456789ab", nil),
			want: buildDetails{Version: "0.0.0-20261121100000-0123456789ab", Commit: "unknown", Date: "unknown"},
		},
		{
			name: "plain go build is a dev build that still knows its commit",
			info: buildInfoWith("(devel)", map[string]string{"vcs.revision": "deadbeef"}),
			want: buildDetails{Version: "dev", Commit: "deadbeef", Date: "unknown"},
		},
		{
			name: "no build info at all",
			info: nil,
			want: buildDetails{Version: "dev", Commit: "unknown", Date: "unknown"},
		},
		{
			name:    "partly linked values fall back per field",
			version: "2.0.0",
			info:    embedded,
			want:    buildDetails{Version: "2.0.0", Commit: "deadbeef", Date: "2026-01-02T03:04:05Z"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveBuild(c.version, c.commit, c.date, c.info); got != c.want {
				t.Errorf("resolveBuild = %+v, want %+v", got, c.want)
			}
		})
	}
}
