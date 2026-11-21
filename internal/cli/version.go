package cli

import (
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// Set at link time: -ldflags "-X github.com/runagent-dev/addition/internal/cli.Version=1.2.3" and likewise Commit, Date.
var (
	Version string
	Commit  string
	Date    string
)

const unknownBuildField = "unknown"

type buildDetails struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

func currentBuild() buildDetails {
	info, _ := debug.ReadBuildInfo()
	return resolveBuild(Version, Commit, Date, info)
}

// resolveBuild prefers linked values, then the Go toolchain's embedded build info, then placeholders.
func resolveBuild(version, commit, date string, info *debug.BuildInfo) buildDetails {
	var moduleVersion, revision, revisionTime string
	if info != nil {
		moduleVersion = info.Main.Version
		revision = buildSetting(info, "vcs.revision")
		revisionTime = buildSetting(info, "vcs.time")
	}
	if moduleVersion == "(devel)" {
		moduleVersion = ""
	}
	return buildDetails{
		Version: strings.TrimPrefix(firstNonEmpty(version, moduleVersion, "dev"), "v"),
		Commit:  firstNonEmpty(commit, revision, unknownBuildField),
		Date:    firstNonEmpty(date, revisionTime, unknownBuildField),
	}
}

func buildSetting(info *debug.BuildInfo, key string) string {
	for _, s := range info.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

type versionOutput struct {
	buildDetails
	Go   string `json:"go"`
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the addition version and build details as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return writeJSON(cmd.OutOrStdout(), versionOutput{
				buildDetails: currentBuild(),
				Go:           runtime.Version(),
				OS:           runtime.GOOS,
				Arch:         runtime.GOARCH,
			})
		},
	}
}
