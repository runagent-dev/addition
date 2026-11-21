package installscript

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

// scriptTools is every external command install.sh may use; tests put only these on PATH.
var scriptTools = []string{
	"awk", "basename", "cat", "chmod", "cp", "dirname", "gzip", "mkdir", "mktemp",
	"mv", "rm", "sed", "tar", "tr", "wc",
}

var platforms = []struct{ os, arch string }{
	{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"},
}

func skipUnlessUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh targets Unix shells")
	}
}

func fakeBinary(version string) []byte {
	return []byte(fmt.Sprintf(`#!/bin/sh
cat >/dev/null
printf '{"version":"%s","commit":"abc","date":"d","go":"go","os":"o","arch":"a"}\n' "%s"
`, version, version))
}

func tarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		mode := int64(0o644)
		if name == "addition" {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(files[name]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarGzBytes(binary string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "addition", Mode: 0o755, Size: int64(len(binary))})
	_, _ = tw.Write([]byte(binary))
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type release struct {
	srv      *httptest.Server
	mu       sync.Mutex
	files    map[string][]byte
	requests []string
}

func newRelease(t *testing.T) *release {
	t.Helper()
	r := &release{files: map[string][]byte{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.requests = append(r.requests, req.URL.Path)
		body, ok := r.files[req.URL.Path]
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *release) set(path string, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[path] = body
}

func (r *release) get(path string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.files[path]
}

func (r *release) remove(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.files, path)
}

func (r *release) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files = map[string][]byte{}
}

// publish serves a release whose binaries report version, under prefix such as /latest/download.
func (r *release) publish(t *testing.T, prefix, version string) {
	t.Helper()
	var sums strings.Builder
	for _, p := range platforms {
		name := fmt.Sprintf("addition_%s_%s.tar.gz", p.os, p.arch)
		archive := tarGz(t, map[string][]byte{
			"addition":  fakeBinary(version),
			"LICENSE":   []byte("license"),
			"NOTICE":    []byte("notice"),
			"README.md": []byte("readme"),
		})
		r.set(prefix+"/"+name, archive)
		fmt.Fprintf(&sums, "%s  %s\n", sha256Hex(archive), name)
	}
	r.set(prefix+"/checksums.txt", []byte(sums.String()))
}

// tamper swaps every archive under prefix for one holding content, leaving checksums.txt as published.
func (r *release) tamper(prefix, content string) {
	for _, p := range platforms {
		r.set(fmt.Sprintf("%s/addition_%s_%s.tar.gz", prefix, p.os, p.arch), tarGzBytes(content))
	}
}

func (r *release) dropArchives(prefix string) {
	for _, p := range platforms {
		r.remove(fmt.Sprintf("%s/addition_%s_%s.tar.gz", prefix, p.os, p.arch))
	}
}

// reseal recomputes checksums.txt under prefix from the archives now served.
func (r *release) reseal(prefix string) {
	var sums strings.Builder
	for _, p := range platforms {
		name := fmt.Sprintf("addition_%s_%s.tar.gz", p.os, p.arch)
		fmt.Fprintf(&sums, "%s  %s\n", sha256Hex(r.get(prefix+"/"+name)), name)
	}
	r.set(prefix+"/checksums.txt", []byte(sums.String()))
}

func (r *release) requested() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...)
}

func unameShim(system, machine string) string {
	return fmt.Sprintf("case \"$1\" in -s) echo %s;; -m) echo %s;; esac\n", system, machine)
}

type toolbox struct{ dir string }

func newToolbox(t *testing.T, names ...string) toolbox {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		src, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("test needs %s on PATH: %v", name, err)
		}
		if err := os.Symlink(src, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return toolbox{dir: dir}
}

func (tb toolbox) writeShim(t *testing.T, name, body string) {
	t.Helper()
	path := filepath.Join(tb.dir, name)
	_ = os.Remove(path)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func hostTool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("needs %s: %v", name, err)
	}
	return path
}

// harness holds one install run's private home, temp dir and tool directory.
type harness struct {
	t       *testing.T
	rel     *release
	tools   toolbox
	home    string
	tmp     string
	install string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	skipUnlessUnix(t)
	tools := newToolbox(t, append([]string{"curl", "sha256sum"}, scriptTools...)...)
	tools.writeShim(t, "uname", unameShim("Linux", "x86_64"))
	root := t.TempDir()
	h := &harness{t: t, rel: newRelease(t), tools: tools, home: filepath.Join(root, "home"), tmp: filepath.Join(root, "tmp"), install: filepath.Join(root, "bin")}
	for _, dir := range []string{h.home, h.tmp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

type invocation struct {
	env   map[string]string
	path  []string
	stdin io.Reader
	shell []string
	dir   string
}

type result struct {
	stdout, stderr string
	code           int
}

func (h *harness) run(mods ...func(*invocation)) result {
	h.t.Helper()
	inv := &invocation{env: map[string]string{
		"HOME":                 h.home,
		"TMPDIR":               h.tmp,
		"ADDITION_BASE_URL":    h.rel.srv.URL,
		"ADDITION_INSTALL_DIR": h.install,
	}, path: []string{h.tools.dir, h.install}}
	for _, mod := range mods {
		mod(inv)
	}
	shell := inv.shell
	if shell == nil {
		shell = []string{hostTool(h.t, "sh")}
	}
	cmd := exec.Command(shell[0], append(append([]string(nil), shell[1:]...), filepath.Join(scriptDir(h.t), "install.sh"))...)
	cmd.Dir = inv.dir
	if cmd.Dir == "" {
		cmd.Dir = scriptDir(h.t)
	}
	cmd.Env = []string{"PATH=" + strings.Join(inv.path, ":")}
	for k, v := range inv.env {
		if v != "-unset-" {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	if inv.stdin != nil {
		cmd.Args = shell
		cmd.Stdin = inv.stdin
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		code = exit.ExitCode()
	case err != nil:
		h.t.Fatalf("running install.sh: %v", err)
	}
	return result{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

func scriptDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func withShell(argv ...string) func(*invocation) { return func(i *invocation) { i.shell = argv } }
func withEnv(k, v string) func(*invocation) { return func(i *invocation) { i.env[k] = v } }
func withPath(dirs ...string) func(*invocation) {
	return func(i *invocation) { i.path = dirs }
}

type success struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
	Path      string `json:"path"`
	OnPath    bool   `json:"on_path"`
	Next      string `json:"next"`
}

func decodeSuccess(t *testing.T, r result) success {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr: %s", r.code, r.stderr)
	}
	if strings.Count(r.stdout, "\n") != 1 || !strings.HasSuffix(r.stdout, "\n") {
		t.Fatalf("stdout must be exactly one line, got %q", r.stdout)
	}
	dec := json.NewDecoder(strings.NewReader(r.stdout))
	dec.DisallowUnknownFields()
	var got success
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("stdout is not the documented JSON: %v\nstdout: %q", err, r.stdout)
	}
	return got
}

type failure struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func assertFailure(t *testing.T, r result, wantCode string, wantExit int) failure {
	t.Helper()
	if r.stdout != "" {
		t.Errorf("stdout = %q, want empty on failure", r.stdout)
	}
	if r.code != wantExit {
		t.Errorf("exit code = %d, want %d\nstderr: %s", r.code, wantExit, r.stderr)
	}
	lines := strings.Split(strings.TrimRight(r.stderr, "\n"), "\n")
	var got failure
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
		t.Fatalf("last stderr line is not an error envelope: %v\nstderr: %s", err, r.stderr)
	}
	if got.Error.Code != wantCode || got.Error.Message == "" {
		t.Errorf("error = %+v, want code %q and a message\nstderr: %s", got.Error, wantCode, r.stderr)
	}
	return got
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func (h *harness) assertCleanTemp() {
	h.t.Helper()
	if left := listDir(h.t, h.tmp); len(left) != 0 {
		h.t.Errorf("temp dir not cleaned up: %v", left)
	}
}

func TestInstallsLatestAndPrintsOneJSONLine(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, "/latest/download", "0.3.0")

	r := h.run()
	got := decodeSuccess(t, r)

	want := success{Installed: true, Version: "0.3.0", Path: filepath.Join(h.install, "addition"), OnPath: true, Next: "addition init"}
	if got != want {
		t.Errorf("stdout = %+v, want %+v", got, want)
	}
	info, err := os.Stat(want.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", info.Mode().Perm())
	}
	if names := listDir(t, h.install); len(names) != 1 || names[0] != "addition" {
		t.Errorf("install dir = %v, want only the binary", names)
	}
	reqs := h.rel.requested()
	sort.Strings(reqs)
	wantRequests := []string{"/latest/download/addition_linux_amd64.tar.gz", "/latest/download/checksums.txt"}
	if strings.Join(reqs, ",") != strings.Join(wantRequests, ",") {
		t.Errorf("requests = %v, want %v", reqs, wantRequests)
	}
	h.assertCleanTemp()
}

func TestPinnedVersionAcceptsAnOptionalLeadingV(t *testing.T) {
	for _, requested := range []string{"v0.1.0", "0.1.0"} {
		t.Run(requested, func(t *testing.T) {
			h := newHarness(t)
			h.rel.publish(t, "/download/v0.1.0", "0.1.0")

			got := decodeSuccess(t, h.run(withEnv("ADDITION_VERSION", requested)))

			if got.Version != "0.1.0" {
				t.Errorf("version = %q, want 0.1.0", got.Version)
			}
			for _, path := range h.rel.requested() {
				if !strings.HasPrefix(path, "/download/v0.1.0/") {
					t.Errorf("requested %q, want it under /download/v0.1.0/", path)
				}
			}
		})
	}
}

func TestPinnedPrereleaseVersion(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, "/download/v0.1.0-rc.1", "0.1.0-rc.1")

	got := decodeSuccess(t, h.run(withEnv("ADDITION_VERSION", "v0.1.0-rc.1")))

	if got.Version != "0.1.0-rc.1" {
		t.Errorf("version = %q", got.Version)
	}
}

func TestRejectsAnInvalidVersionBeforeAnyRequest(t *testing.T) {
	for _, bad := range []string{
		"latest-ish/../x", "1.0", "v", "1.0.0 ", "$(id)", "1abc.2abc.3abc", "1.2", "1.2.3.4", "1..3", ".1.2",
		"1.2.3-", "1.2.3-rc_1", "1.2.3-rc-1", "vv1.2.3", "1.2.3+build", "bad\tversion", "1.2.3\n",
	} {
		t.Run(bad, func(t *testing.T) {
			h := newHarness(t)

			r := h.run(withEnv("ADDITION_VERSION", bad))

			assertFailure(t, r, "invalid-version", 2)
			if reqs := h.rel.requested(); len(reqs) != 0 {
				t.Errorf("requests = %v, want none", reqs)
			}
		})
	}
}

func TestDefaultInstallDirIsUnderHome(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, "/latest/download", "0.3.0")

	got := decodeSuccess(t, h.run(withEnv("ADDITION_INSTALL_DIR", "-unset-")))

	want := filepath.Join(h.home, ".local", "bin", "addition")
	if got.Path != want {
		t.Errorf("path = %q, want %q", got.Path, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Error(err)
	}
}

func TestOnPathIsFalseAndStderrExplainsHowToFixIt(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, "/latest/download", "0.3.0")

	r := h.run(withPath(h.tools.dir))
	got := decodeSuccess(t, r)

	if got.OnPath {
		t.Error("on_path = true, want false when the install dir is not on PATH")
	}
	if !strings.Contains(r.stderr, `export PATH='`+h.install+`':"$PATH"`) {
		t.Errorf("stderr does not say how to add the directory to PATH:\n%s", r.stderr)
	}
	for _, rc := range []string{".bashrc", ".zshrc", ".profile"} {
		if _, err := os.Stat(filepath.Join(h.home, rc)); err == nil {
			t.Errorf("%s was created; the installer must not edit shell rc files", rc)
		}
	}
}

func TestOnPathIsFalseWhenAnotherAdditionShadowsIt(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, "/latest/download", "0.3.0")
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "addition"), fakeBinary("0.0.1"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := decodeSuccess(t, h.run(withPath(h.tools.dir, other, h.install)))

	if got.OnPath {
		t.Error("on_path = true although an older addition comes first on PATH")
	}
}

func TestChecksumMismatchRefusesAndLeavesTheInstallDirUntouched(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, "/latest/download", "0.3.0")
	h.rel.tamper("/latest/download", "evil")

	r := h.run()

	assertFailure(t, r, "checksum-mismatch", 1)
	if names := listDir(t, h.install); names != nil {
		t.Errorf("install dir was created or touched: %v", names)
	}
	h.assertCleanTemp()
}

func TestChecksumMismatchKeepsAnExistingBinary(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, "/latest/download", "0.3.0")
	h.rel.tamper("/latest/download", "tampered")
	if err := os.MkdirAll(h.install, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(h.install, "addition")
	if err := os.WriteFile(existing, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	assertFailure(t, h.run(), "checksum-mismatch", 1)

	if got, _ := os.ReadFile(existing); string(got) != "old binary" {
		t.Errorf("existing binary = %q, want it untouched", got)
	}
	if names := listDir(t, h.install); len(names) != 1 {
		t.Errorf("install dir = %v, want only the old binary", names)
	}
}

func TestMissingChecksumEntryRefuses(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, "/latest/download", "0.3.0")
	h.rel.set("/latest/download/checksums.txt", []byte(sha256Hex([]byte("x"))+"  addition_plan9_mips.tar.gz\n"))

	r := h.run()

	assertFailure(t, r, "checksum-missing", 1)
	if names := listDir(t, h.install); names != nil {
		t.Errorf("install dir was created: %v", names)
	}
	h.assertCleanTemp()
}

func TestHTTPErrorsAreDownloadFailures(t *testing.T) {
	cases := map[string]func(h *harness){
		"archive 404":       func(h *harness) { h.rel.dropArchives("/latest/download") },
		"checksums 404":     func(h *harness) { h.rel.remove("/latest/download/checksums.txt") },
		"nothing published": func(h *harness) { h.rel.clear() },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.rel.publish(t, "/latest/download", "0.3.0")
			breakIt(h)

			r := h.run()

			assertFailure(t, r, "download-failed", 5)
			h.assertCleanTemp()
		})
	}
}

func TestUnreachableServerIsADownloadFailure(t *testing.T) {
	h := newHarness(t)
	h.rel.srv.Close()

	assertFailure(t, h.run(), "download-failed", 5)
}

func TestPlatformDetectionPicksTheMatchingAsset(t *testing.T) {
	cases := []struct{ uname, machine, asset string }{
		{"Linux", "x86_64", "addition_linux_amd64.tar.gz"},
		{"Linux", "aarch64", "addition_linux_arm64.tar.gz"},
		{"Linux", "arm64", "addition_linux_arm64.tar.gz"},
		{"Darwin", "x86_64", "addition_darwin_amd64.tar.gz"},
		{"Darwin", "arm64", "addition_darwin_arm64.tar.gz"},
	}
	for _, c := range cases {
		t.Run(c.uname+"/"+c.machine, func(t *testing.T) {
			h := newHarness(t)
			h.rel.publish(t, "/latest/download", "0.3.0")
			h.tools.writeShim(t, "uname", unameShim(c.uname, c.machine))

			decodeSuccess(t, h.run())

			if reqs := h.rel.requested(); !contains(reqs, "/latest/download/"+c.asset) {
				t.Errorf("requests = %v, want %s", reqs, c.asset)
			}
		})
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestUnsupportedPlatformsAreRefusedBeforeAnyRequest(t *testing.T) {
	cases := []struct{ name, uname, machine, code, mention string }{
		{"windows git-bash", "MINGW64_NT-10.0", "x86_64", "unsupported-os", "releases"},
		{"freebsd", "FreeBSD", "amd64", "unsupported-os", "releases"},
		{"386 linux", "Linux", "i686", "unsupported-arch", "i686"},
		{"riscv", "Linux", "riscv64", "unsupported-arch", "riscv64"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.tools.writeShim(t, "uname", unameShim(c.uname, c.machine))

			got := assertFailure(t, h.run(), c.code, 2)

			if !strings.Contains(got.Error.Message, c.mention) {
				t.Errorf("message %q should mention %q", got.Error.Message, c.mention)
			}
			if reqs := h.rel.requested(); len(reqs) != 0 {
				t.Errorf("requests = %v, want none", reqs)
			}
		})
	}
}

func TestWgetIsUsedWhenThereIsNoCurl(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, "/latest/download", "0.3.0")
	curl := hostTool(t, "curl")
	_ = os.Remove(filepath.Join(h.tools.dir, "curl"))
	h.tools.writeShim(t, "wget", fmt.Sprintf(`[ "$1" = -q ] && [ "$2" = -O ] && [ "$4" = -- ] || { echo "unexpected wget args: $*" >&2; exit 3; }
exec %s -fsSL -o "$3" -- "$5"
`, curl))

	got := decodeSuccess(t, h.run())

	if got.Version != "0.3.0" {
		t.Errorf("version = %q", got.Version)
	}
}

func TestNoCurlAndNoWgetIsAnError(t *testing.T) {
	h := newHarness(t)
	_ = os.Remove(filepath.Join(h.tools.dir, "curl"))

	assertFailure(t, h.run(), "no-downloader", 1)
	if reqs := h.rel.requested(); len(reqs) != 0 {
		t.Errorf("requests = %v, want none", reqs)
	}
}

func TestShasumIsUsedWhenThereIsNoSha256sum(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, "/latest/download", "0.3.0")
	real := hostTool(t, "sha256sum")
	_ = os.Remove(filepath.Join(h.tools.dir, "sha256sum"))
	h.tools.writeShim(t, "shasum", fmt.Sprintf(`[ "$1" = -a ] && [ "$2" = 256 ] || { echo "unexpected shasum args: $*" >&2; exit 3; }
shift 2
exec %s "$@"
`, real))

	decodeSuccess(t, h.run())
}

func TestNoChecksumToolIsAnError(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, "/latest/download", "0.3.0")
	_ = os.Remove(filepath.Join(h.tools.dir, "sha256sum"))

	assertFailure(t, h.run(), "no-checksum-tool", 1)
	if names := listDir(t, h.install); names != nil {
		t.Errorf("install dir was created: %v", names)
	}
}

func TestRerunUpgradesInPlaceWithoutLeftovers(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, "/download/v0.1.0", "0.1.0")
	h.rel.publish(t, "/download/v0.2.0", "0.2.0")

	first := decodeSuccess(t, h.run(withEnv("ADDITION_VERSION", "0.1.0")))
	second := decodeSuccess(t, h.run(withEnv("ADDITION_VERSION", "0.2.0")))

	if first.Version != "0.1.0" || second.Version != "0.2.0" {
		t.Errorf("versions = %q then %q", first.Version, second.Version)
	}
	if names := listDir(t, h.install); len(names) != 1 || names[0] != "addition" {
		t.Errorf("install dir = %v, want only the binary", names)
	}
	h.assertCleanTemp()
}

func TestInstallFailureReportsAndLeavesNoTemporaryFiles(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, "/latest/download", "0.3.0")
	if err := os.WriteFile(h.install, []byte("a file, not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	assertFailure(t, h.run(), "install-failed", 1)
	h.assertCleanTemp()
}

func TestInstalledBinaryIsNotRunWhenItCannotBeExecuted(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, "/latest/download", "0.3.0")
	h.rel.tamper("/latest/download", "\x7fELF-not-runnable")
	h.rel.reseal("/latest/download")

	got := decodeSuccess(t, h.run())

	if got.Version != "latest" {
		t.Errorf("version = %q, want the requested version when the binary cannot report one", got.Version)
	}
}

func TestWorksWhenThePipedScriptIsOnStdin(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, "/latest/download", "0.3.0")
	script, err := os.ReadFile(filepath.Join(scriptDir(t), "install.sh"))
	if err != nil {
		t.Fatal(err)
	}

	got := decodeSuccess(t, h.run(func(i *invocation) { i.stdin = bytes.NewReader(script) }))

	if got.Version != "0.3.0" || !got.OnPath {
		t.Errorf("stdout = %+v", got)
	}
}

func TestOnlyTheBinaryIsExtracted(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, "/latest/download", "0.3.0")

	decodeSuccess(t, h.run())

	for _, extra := range []string{"LICENSE", "NOTICE", "README.md"} {
		if _, err := os.Stat(filepath.Join(h.install, extra)); err == nil {
			t.Errorf("%s was installed next to the binary", extra)
		}
	}
}

func TestNeverCallsSudo(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, "/latest/download", "0.3.0")
	h.tools.writeShim(t, "sudo", "echo sudo was called >&2; exit 99\n")

	r := h.run()

	if decodeSuccess(t, r); strings.Contains(r.stderr, "sudo was called") {
		t.Error("install.sh called sudo")
	}
}
