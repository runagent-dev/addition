package installscript

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const latest = "/latest/download"
const linuxArchive = latest + "/addition_linux_amd64.tar.gz"

func (h *harness) assertNothingInstalled() {
	h.t.Helper()
	if names := listDir(h.t, h.install); names != nil {
		h.t.Errorf("install dir was created or touched: %v", names)
	}
	h.assertCleanTemp()
}

type entry struct {
	hdr  tar.Header
	body []byte
}

func regular(name, body string) entry {
	return entry{hdr: tar.Header{Name: name, Mode: 0o755, Typeflag: tar.TypeReg}, body: []byte(body)}
}

func rawTarGz(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		e.hdr.Size = int64(len(e.body))
		if err := tw.WriteHeader(&e.hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return gzipBytes(t, &buf)
}

func gzipBytes(t *testing.T, in io.Reader) []byte {
	t.Helper()
	cmd := exec.Command(hostTool(t, "gzip"), "-1", "-n")
	var out bytes.Buffer
	cmd.Stdin, cmd.Stdout = in, &out
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// archiveWithBinarySize is a valid tar.gz whose `addition` member is size bytes of zeros.
func archiveWithBinarySize(t *testing.T, size int64) []byte {
	t.Helper()
	var header bytes.Buffer
	if err := tar.NewWriter(&header).WriteHeader(&tar.Header{Name: "addition", Mode: 0o755, Size: size, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	return gzipBytes(t, io.MultiReader(&header, io.LimitReader(zeros{}, size), io.LimitReader(zeros{}, 1024)))
}

func (h *harness) serveArchive(archive []byte) {
	h.rel.set(linuxArchive, archive)
	h.rel.reseal(latest)
}

func TestTruncatedScriptNeverInstalls(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, latest, "0.3.0")
	script, err := os.ReadFile(filepath.Join(scriptDir(t), "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	finalStatement := bytes.LastIndex(script, []byte("\n{\n")) + 1
	if finalStatement == 0 {
		t.Fatal("install.sh must end in a { ... } group that only parses when complete")
	}

	for cut := finalStatement; cut < len(script)-1; cut++ {
		r := h.run(func(i *invocation) { i.stdin = bytes.NewReader(script[:cut]) })

		if r.stdout != "" || (cut > finalStatement && r.code == 0) {
			t.Fatalf("script cut at byte %d of %d: exit %d, stdout %q\nstderr: %s", cut, len(script), r.code, r.stdout, r.stderr)
		}
		if reqs := h.rel.requested(); len(reqs) != 0 {
			t.Fatalf("script cut at byte %d reached the network: %v", cut, reqs)
		}
		h.assertNothingInstalled()
	}
}

func TestRefusesADirectoryAsTheDestination(t *testing.T) {
	cases := map[string]func(t *testing.T, install string) (inside string){
		"directory": func(t *testing.T, install string) string {
			dir := filepath.Join(install, "addition")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			return dir
		},
		"symlink to directory": func(t *testing.T, install string) string {
			real := filepath.Join(install, "real")
			if err := os.MkdirAll(real, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, filepath.Join(install, "addition")); err != nil {
				t.Fatal(err)
			}
			return real
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.rel.publish(t, latest, "0.3.0")
			inside := setup(t, h.install)
			before := listDir(t, h.install)

			assertFailure(t, h.run(), "install-failed", 1)

			if left := listDir(t, inside); left != nil && len(left) != 0 {
				t.Errorf("something was moved into the directory: %v", left)
			}
			if after := listDir(t, h.install); strings.Join(after, ",") != strings.Join(before, ",") {
				t.Errorf("install dir = %v, want %v", after, before)
			}
			h.assertCleanTemp()
		})
	}
}

func TestReplacesASymlinkToAFileWithoutWritingThroughIt(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, latest, "0.3.0")
	if err := os.MkdirAll(h.install, 0o755); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "other")
	if err := os.WriteFile(elsewhere, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(h.install, "addition")); err != nil {
		t.Fatal(err)
	}

	decodeSuccess(t, h.run())

	if got, _ := os.ReadFile(elsewhere); string(got) != "keep me" {
		t.Errorf("the symlink target was overwritten: %q", got)
	}
	info, err := os.Lstat(filepath.Join(h.install, "addition"))
	if err != nil || !info.Mode().IsRegular() {
		t.Errorf("destination = %v, %v; want a regular file", info, err)
	}
}

func TestRejectsArchivesWithoutExactlyOneRegularBinary(t *testing.T) {
	cases := map[string][]entry{
		"symlink named addition": {{hdr: tar.Header{Name: "addition", Typeflag: tar.TypeSymlink, Linkname: "/etc/hostname"}}},
		"hardlink named addition": {
			regular("LICENSE", "license"),
			{hdr: tar.Header{Name: "addition", Typeflag: tar.TypeLink, Linkname: "LICENSE"}},
		},
		"directory named addition":   {{hdr: tar.Header{Name: "addition/", Mode: 0o755, Typeflag: tar.TypeDir}}},
		"binary plus a directory":    {regular("addition", "bin"), {hdr: tar.Header{Name: "extra/", Mode: 0o755, Typeflag: tar.TypeDir}}},
		"binary plus a symlink":      {regular("addition", "bin"), {hdr: tar.Header{Name: "LICENSE", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}}},
		"absolute path member":       {regular("addition", "bin"), regular("/tmp/addition-evil", "x")},
		"parent directory member":    {regular("addition", "bin"), regular("../evil", "x")},
		"nested parent member":       {regular("addition", "bin"), regular("a/../../evil", "x")},
		"two binaries":               {regular("addition", "one"), regular("addition", "two")},
		"no binary":                  {regular("LICENSE", "license")},
		"binary only below a folder": {regular("bin/addition", "bin")},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.rel.publish(t, latest, "0.3.0")
			h.serveArchive(rawTarGz(t, entries...))

			assertFailure(t, h.run(), "archive-invalid", 1)

			h.assertNothingInstalled()
		})
	}
}

func TestOtherRegularMembersAreIgnored(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, latest, "0.3.0")
	h.serveArchive(rawTarGz(t, regular("LICENSE", "l"), regular("addition", string(fakeBinary("0.9.9"))), regular("docs.txt", "d")))

	if got := decodeSuccess(t, h.run()); got.Version != "0.9.9" {
		t.Errorf("version = %q", got.Version)
	}
}

func TestRejectsABinaryAboveTheSizeCap(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, latest, "0.3.0")
	h.serveArchive(archiveWithBinarySize(t, 201<<20))

	assertFailure(t, h.run(), "binary-too-large", 1)

	h.assertNothingInstalled()
}

func TestRejectsDownloadsAboveTheSizeCaps(t *testing.T) {
	cases := map[string]string{
		"archive":       linuxArchive,
		"checksums.txt": latest + "/checksums.txt",
	}
	sizes := map[string]int{"archive": 101 << 20, "checksums.txt": 2 << 20}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.rel.publish(t, latest, "0.3.0")
			h.rel.set(path, make([]byte, sizes[name]))

			assertFailure(t, h.run(), "download-too-large", 5)

			h.assertNothingInstalled()
		})
	}
}

func TestRejectsUnsafeBaseURLs(t *testing.T) {
	bad := []string{
		"-o/tmp/pwned", "--config=/etc/passwd", "ftp://example.com", "file:///etc", "http://example.com",
		"http://127.0.0.1.example.com", "http://localhost@example.com/", "https://user@example.com", "https://",
		"example.com/releases", "https://example.com/a b", "https://example.com/?x=1", "https://example.com/#f",
		"https://example.com\\@evil.test", "https://example.com/\nx",
	}
	for _, url := range bad {
		t.Run(url, func(t *testing.T) {
			h := newHarness(t)
			h.rel.publish(t, latest, "0.3.0")

			assertFailure(t, h.run(withEnv("ADDITION_BASE_URL", url)), "invalid-base-url", 2)

			if reqs := h.rel.requested(); len(reqs) != 0 {
				t.Errorf("requests = %v, want none", reqs)
			}
			h.assertNothingInstalled()
		})
	}
}

func TestAcceptsLoopbackHTTPBaseURLs(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "localhost"} {
		t.Run(host, func(t *testing.T) {
			h := newHarness(t)
			h.rel.publish(t, latest, "0.3.0")
			port := h.rel.srv.URL[strings.LastIndex(h.rel.srv.URL, ":")+1:]

			decodeSuccess(t, h.run(withEnv("ADDITION_BASE_URL", "http://"+host+":"+port+"/")))
		})
	}
}

func TestDownloadersGetEachURLAfterEndOfOptions(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, latest, "0.3.0")
	curl := hostTool(t, "curl")
	_ = os.Remove(filepath.Join(h.tools.dir, "curl"))
	log := filepath.Join(t.TempDir(), "args")
	h.tools.writeShim(t, "curl", fmt.Sprintf("printf '%%s\\n' \"$*\" >>%q\nexec %s \"$@\"\n", log, curl))

	decodeSuccess(t, h.run())

	data, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("curl calls = %q", lines)
	}
	for _, line := range lines {
		if !strings.Contains(line, " -- http://") {
			t.Errorf("curl %s: the URL must follow --", line)
		}
	}
}

func TestChecksumRecordMustBeExactlyOneWellFormedLine(t *testing.T) {
	name := "addition_linux_amd64.tar.gz"
	h := newHarness(t)
	h.rel.publish(t, latest, "0.3.0")
	good := sha256Hex(h.rel.get(linuxArchive))
	other := sha256Hex([]byte("other"))
	cases := []struct {
		label, sums, code string
	}{
		{"duplicate with a different digest", good + "  " + name + "\n" + other + "  " + name + "\n", "checksum-malformed"},
		{"duplicate identical", good + "  " + name + "\n" + good + "  " + name + "\n", "checksum-malformed"},
		{"extra field", good + "  " + name + " extra\n", "checksum-malformed"},
		{"short digest", good[:63] + "  " + name + "\n", "checksum-malformed"},
		{"long digest", good + "0  " + name + "\n", "checksum-malformed"},
		{"non-hex digest", "z" + good[1:] + "  " + name + "\n", "checksum-malformed"},
		{"one space", good + " " + name + "\n", "checksum-malformed"},
		{"tab", good + "\t" + name + "\n", "checksum-malformed"},
		{"no entry", good + "  addition_plan9_mips.tar.gz\n", "checksum-missing"},
		{"empty", "", "checksum-missing"},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			h := newHarness(t)
			h.rel.publish(t, latest, "0.3.0")
			h.rel.set(latest+"/checksums.txt", []byte(c.sums))

			assertFailure(t, h.run(), c.code, 1)

			h.assertNothingInstalled()
		})
	}

	accepted := map[string]string{
		"binary mode":      good + " *" + name + "\n",
		"upper-case hex":   strings.ToUpper(good) + "  " + name + "\n",
		"other names too":  other + "  addition_darwin_arm64.tar.gz\n" + good + "  " + name + "\n" + other + "  checksums.txt.sig\n",
		"no final newline": good + "  " + name,
	}
	for label, sums := range accepted {
		t.Run(label, func(t *testing.T) {
			h := newHarness(t)
			h.rel.publish(t, latest, "0.3.0")
			h.rel.set(latest+"/checksums.txt", []byte(sums))

			decodeSuccess(t, h.run())
		})
	}
}

func TestChecksumToolFailureIsReportedNotMasked(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, latest, "0.3.0")
	h.tools.writeShim(t, "sha256sum", "exit 3\n")

	assertFailure(t, h.run(), "checksum-failed", 1)

	h.assertNothingInstalled()
}

func TestTempDirNamesCannotChangeTheDigest(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, latest, "0.3.0")
	odd := filepath.Join(h.tmp, `back\slash`)
	if err := os.MkdirAll(odd, 0o755); err != nil {
		t.Fatal(err)
	}

	decodeSuccess(t, h.run(withEnv("TMPDIR", odd)))

	if left := listDir(t, odd); len(left) != 0 {
		t.Errorf("temp dir not cleaned up: %v", left)
	}
}

func TestRelativeTempAndInstallDirsWork(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, latest, "0.3.0")
	work := t.TempDir()
	if err := os.Mkdir(filepath.Join(work, "-tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := h.run(
		withEnv("TMPDIR", "-tmp"),
		withEnv("ADDITION_INSTALL_DIR", "-bin"),
		func(i *invocation) { i.dir = work },
	)

	got := decodeSuccess(t, r)

	if want := filepath.Join(work, "-bin", "addition"); got.Path != want {
		t.Errorf("path = %q, want %q", got.Path, want)
	}
}

func TestJSONStaysValidForHostileInstallPaths(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, latest, "0.3.0")
	dir := filepath.Join(h.install, "tab\tquote\"back\\slash'apostrophe\nnewline\x01ctl")

	r := h.run(withEnv("ADDITION_INSTALL_DIR", dir))
	got := decodeSuccess(t, r)

	if want := filepath.Join(dir, "addition"); got.Path != want {
		t.Errorf("path = %q, want %q", got.Path, want)
	}
	hint := `export PATH='` + strings.ReplaceAll(dir, "'", `'\''`) + `':"$PATH"`
	if !strings.Contains(r.stderr, hint) {
		t.Errorf("stderr lacks the shell-quoted PATH hint %q:\n%s", hint, r.stderr)
	}
}

func TestPATHHintRunsInAShellAsExactlyThePathItNames(t *testing.T) {
	h := newHarness(t)
	h.rel.publish(t, latest, "0.3.0")
	dir := filepath.Join(h.install, "has space $(touch pwned) `x` 'q'")
	r := h.run(withEnv("ADDITION_INSTALL_DIR", dir), func(i *invocation) { i.dir = h.home })
	decodeSuccess(t, r)
	var hint string
	for _, line := range strings.Split(r.stderr, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "export PATH=") {
			hint = strings.TrimSpace(line)
		}
	}

	out, err := exec.Command(hostTool(t, "sh"), "-c", hint+`; printf %s "${PATH%%:*}"`).CombinedOutput()

	if err != nil || string(out) != dir {
		t.Errorf("hint put %q on PATH (err %v), want %q", out, err, dir)
	}
	if _, err := os.Stat(filepath.Join(h.home, "pwned")); err == nil {
		t.Error("the PATH hint executed a command substitution")
	}
}

func TestErrorEnvelopeEscapesEveryControlCharacter(t *testing.T) {
	h := newHarness(t)
	version := "bad\tver\"sion\\ \x01 \r\nend"

	r := h.run(withEnv("ADDITION_VERSION", version))

	got := assertFailure(t, r, "invalid-version", 2)
	if !strings.Contains(got.Error.Message, version) {
		t.Errorf("message = %q, want it to carry the exact version %q", got.Error.Message, version)
	}
}

func TestAcceptsOnlyReleaseVersionSyntax(t *testing.T) {
	for _, v := range []string{"0.1.0", "v1.20.300", "v1.2.3-rc.1", "1.2.3-beta", "10.0.0-0.3.7"} {
		t.Run(v, func(t *testing.T) {
			h := newHarness(t)
			tag := "v" + strings.TrimPrefix(v, "v")
			h.rel.publish(t, "/download/"+tag, strings.TrimPrefix(v, "v"))

			decodeSuccess(t, h.run(withEnv("ADDITION_VERSION", v)))
		})
	}
}

func TestInstallsUnderEveryAvailableShell(t *testing.T) {
	shells := [][]string{{"sh"}, {"dash"}, {"bash"}, {"busybox", "sh"}}
	for _, argv := range shells {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			path, err := exec.LookPath(argv[0])
			if err != nil {
				t.Skipf("%s not installed", argv[0])
			}
			h := newHarness(t)
			h.rel.publish(t, latest, "0.3.0")

			got := decodeSuccess(t, h.run(withShell(append([]string{path}, argv[1:]...)...)))

			if got.Version != "0.3.0" {
				t.Errorf("version = %q", got.Version)
			}
			h.assertCleanTemp()
		})
	}
}

func TestInstallsWithTheRealWget(t *testing.T) {
	wget, err := exec.LookPath("wget")
	if err != nil {
		t.Skip("wget not installed")
	}
	h := newHarness(t)
	h.rel.publish(t, latest, "0.3.0")
	_ = os.Remove(filepath.Join(h.tools.dir, "curl"))
	if err := os.Symlink(wget, filepath.Join(h.tools.dir, "wget")); err != nil {
		t.Fatal(err)
	}

	decodeSuccess(t, h.run())
}
