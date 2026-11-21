package installscript

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type workflow struct {
	Env  map[string]string `yaml:"env"`
	Jobs map[string]struct {
		Permissions map[string]string `yaml:"permissions"`
		Steps       []struct {
			Name string            `yaml:"name"`
			Uses string            `yaml:"uses"`
			Run  string            `yaml:"run"`
			Env  map[string]string `yaml:"env"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func loadWorkflow(t *testing.T) workflow {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(scriptDir(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}
	return wf
}

func TestReleaseWorkflowBindsGoReleaserToTheEventTag(t *testing.T) {
	build := loadWorkflow(t).Jobs["build"]
	gorelease, check, upload := -1, -1, -1
	for i, step := range build.Steps {
		switch {
		case strings.Contains(step.Run, "goreleaser release"):
			gorelease = i
			if step.Env["GORELEASER_CURRENT_TAG"] != "${{ github.ref_name }}" {
				t.Errorf("goreleaser step env = %v, want GORELEASER_CURRENT_TAG from github.ref_name", step.Env)
			}
			if !strings.Contains(step.Run, "-e GORELEASER_CURRENT_TAG") {
				t.Error("the tag is not passed into the goreleaser container")
			}
			if strings.Contains(step.Run, "GITHUB_TOKEN") {
				t.Error("goreleaser must not see a GitHub token")
			}
		case strings.Contains(step.Run, "scripts/check-release-dist.sh"):
			check = i
			if step.Env["TAG"] != "${{ github.ref_name }}" || !strings.Contains(step.Run, `"$TAG"`) {
				t.Errorf("check step must read the tag from env: %+v", step)
			}
		case strings.HasPrefix(step.Uses, "actions/upload-artifact@"):
			upload = i
		}
	}
	if gorelease < 0 || check < 0 || upload < 0 || !(gorelease < check && check < upload) {
		t.Errorf("want goreleaser (%d), then the dist check (%d), then upload (%d)", gorelease, check, upload)
	}
}

func TestReleaseWorkflowNeverExpandsContextsInsideScripts(t *testing.T) {
	for name, job := range loadWorkflow(t).Jobs {
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "${{") {
				t.Errorf("%s: step %q expands an expression inside its script", name, step.Name)
			}
		}
	}
}

func TestOnlyPublishCanWriteContents(t *testing.T) {
	for name, job := range loadWorkflow(t).Jobs {
		writes := job.Permissions["contents"] == "write"
		if writes != (name == "publish") {
			t.Errorf("job %s: contents write = %v", name, writes)
		}
	}
}

var versionTable = []struct {
	version string
	valid   bool
}{
	{"0.1.0", true}, {"1.20.300", true}, {"10.0.0", true},
	{"1.2.3-rc.1", true}, {"1.2.3-rc-1", true}, {"1.2.3-0", true}, {"1.2.3-0a", true},
	{"1.0.0-x.7.z.92", true}, {"1.0.0-alpha.beta-1", true}, {"1.0.0-0.3.7", true},
	{"01.2.3", false}, {"1.02.3", false}, {"1.2.03", false},
	{"1.2.3-.rc", false}, {"1.2.3-rc..1", false}, {"1.2.3-", false}, {"1.2.3-rc.", false},
	{"1.2.3-01", false}, {"1.2.3-rc.01", false}, {"1.2.3-rc_1", false}, {"1.2.3+build", false},
	{"1.2", false}, {"1.2.3.4", false}, {"1abc.2abc.3abc", false}, {"1..3", false}, {"", false},
	{"v1.2.3", false}, {"1.2.3 ", false}, {"1.2.3\n", false},
}

func TestTagRegexInTheWorkflowFollowsTheVersionTable(t *testing.T) {
	regex := loadWorkflow(t).Env["RELEASE_TAG_REGEX"]
	if regex == "" {
		t.Fatal("release.yml must define RELEASE_TAG_REGEX")
	}
	bash := hostTool(t, "bash")
	for _, c := range versionTable {
		tag := "v" + c.version
		err := exec.Command(bash, "-c", `[[ "$1" =~ $2 ]]`, "_", tag, regex).Run()
		if (err == nil) != c.valid {
			t.Errorf("workflow regex on %q: accepted = %v, want %v", tag, err == nil, c.valid)
		}
	}
}

func TestInstallerAndWorkflowShareOneVersionPolicy(t *testing.T) {
	script, err := os.ReadFile(filepath.Join(scriptDir(t), "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*VERSION_REGEX='(.*)'$`).FindSubmatch(script)
	if m == nil {
		t.Fatal("install.sh must define VERSION_REGEX")
	}
	installer := strings.Replace(string(m[1]), "^v?", "^v", 1)
	if workflow := loadWorkflow(t).Env["RELEASE_TAG_REGEX"]; installer != workflow {
		t.Errorf("installer regex %q differs from the workflow's %q", installer, workflow)
	}

	for _, c := range versionTable {
		if c.version == "" || strings.HasPrefix(c.version, "v") {
			continue
		}
		h := newHarness(t)
		r := h.run(withEnv("ADDITION_VERSION", c.version))
		rejected := strings.Contains(r.stderr, `"code":"invalid-version"`)
		if rejected == c.valid {
			t.Errorf("installer on %q: rejected = %v, want %v", c.version, rejected, !c.valid)
		}
	}
}

func distDir(t *testing.T, tag, binaryVersion string) string {
	t.Helper()
	dir := t.TempDir()
	var sums strings.Builder
	for _, name := range []string{
		"addition_darwin_amd64.tar.gz", "addition_darwin_arm64.tar.gz", "addition_linux_amd64.tar.gz",
		"addition_linux_arm64.tar.gz", "addition_windows_amd64.zip", "addition_windows_arm64.zip",
	} {
		body := []byte("archive " + name)
		if name == "addition_linux_amd64.tar.gz" {
			body = tarGz(t, map[string][]byte{"addition": fakeBinary(binaryVersion), "LICENSE": []byte("l")})
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&sums, "%s  %s\n", sha256Hex(body), name)
	}
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(sums.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runDistCheck(t *testing.T, dir, tag string) (string, error) {
	t.Helper()
	cmd := exec.Command(hostTool(t, "bash"), filepath.Join(scriptDir(t), "scripts", "check-release-dist.sh"), dir, tag)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func TestDistCheckAcceptsTheExpectedAssets(t *testing.T) {
	for _, tag := range []string{"v0.1.0", "v0.1.0-rc.1"} {
		if out, err := runDistCheck(t, distDir(t, tag, strings.TrimPrefix(tag, "v")), tag); err != nil {
			t.Errorf("%s: %v\n%s", tag, err, out)
		}
	}
}

func TestDistCheckRefusesAWrongBuild(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"binary built for another tag": nil,
		"archive missing": func(t *testing.T, dir string) {
			_ = os.Remove(filepath.Join(dir, "addition_windows_arm64.zip"))
		},
		"checksums.txt missing": func(t *testing.T, dir string) { _ = os.Remove(filepath.Join(dir, "checksums.txt")) },
		"checksums.txt lists an extra file": func(t *testing.T, dir string) {
			f, _ := os.OpenFile(filepath.Join(dir, "checksums.txt"), os.O_APPEND|os.O_WRONLY, 0)
			fmt.Fprintf(f, "%s  extra.bin\n", sha256Hex([]byte("x")))
			f.Close()
		},
		"checksums.txt lacks an archive": func(t *testing.T, dir string) {
			data, _ := os.ReadFile(filepath.Join(dir, "checksums.txt"))
			lines := strings.SplitAfter(string(data), "\n")
			_ = os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(strings.Join(lines[1:], "")), 0o644)
		},
		"archive changed after checksumming": func(t *testing.T, dir string) {
			_ = os.WriteFile(filepath.Join(dir, "addition_darwin_arm64.tar.gz"), []byte("tampered"), 0o644)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir := distDir(t, "v0.1.0", "0.2.0")
			if mutate != nil {
				dir = distDir(t, "v0.1.0", "0.1.0")
				mutate(t, dir)
			}
			if out, err := runDistCheck(t, dir, "v0.1.0"); err == nil {
				t.Errorf("check passed, want a failure\n%s", out)
			}
		})
	}
}
