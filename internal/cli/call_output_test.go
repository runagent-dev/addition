package cli

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type savedFile struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Bytes       int64  `json:"bytes"`
	Path        string `json:"path"`
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestCallSavesABinaryBodyToTheOutputFile(t *testing.T) {
	c := newCallHarness(t)
	payload := append([]byte("%PDF-1.7\x00\x01\x02"), bytes.Repeat([]byte{0xff, 0x00}, 100)...)
	c.api.respond(status(200, string(payload), "Content-Type", "application/pdf"))
	dir := t.TempDir()
	target := filepath.Join(dir, "invoice.pdf")

	r := c.mustRun("call", "-a", "work", "GET", "/drive/v3/files/1?alt=media", "-o", target)

	got := decodeJSON[savedFile](t, r)
	if got.Status != 200 || got.ContentType != "application/pdf" || got.Bytes != int64(len(payload)) || got.Path != target {
		t.Errorf("summary = %+v", got)
	}
	data, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(data, payload) {
		t.Errorf("file = %d bytes, %v; want the body verbatim", len(data), err)
	}
	if names := dirNames(t, dir); !slices.Equal(names, []string{"invoice.pdf"}) {
		t.Errorf("directory = %v, want no temp files left behind", names)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q", r.stderr)
	}
}

func TestCallStreamsALargeBodyToTheOutputFile(t *testing.T) {
	c := newCallHarness(t)
	chunk := bytes.Repeat([]byte("0123456789abcdef"), 1<<10)
	const chunks = 320
	want := bytes.Repeat(chunk, chunks)
	c.api.respond(func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		for range chunks {
			_, _ = w.Write(chunk)
		}
	})
	target := filepath.Join(t.TempDir(), "big.bin")

	r := c.mustRun("call", "-a", "work", "GET", "/x", "-o", target)

	got := decodeJSON[savedFile](t, r)
	data, _ := os.ReadFile(target)
	if got.Bytes != int64(len(want)) || sha(data) != sha(want) {
		t.Errorf("saved %d bytes (reported %d), want %d identical bytes", len(data), got.Bytes, len(want))
	}
}

func TestCallSavesAnEmptyBodyToTheOutputFile(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(status(204, ""))
	target := filepath.Join(t.TempDir(), "empty")

	r := c.mustRun("call", "-a", "work", "DELETE", "/x", "-o", target)

	if got := decodeJSON[savedFile](t, r); got.Status != 204 || got.Bytes != 0 {
		t.Errorf("summary = %+v", got)
	}
	if info, err := os.Stat(target); err != nil || info.Size() != 0 {
		t.Errorf("file: %v, %v", info, err)
	}
}

func TestCallRefusesToOverwriteWithoutForceAndNeverSends(t *testing.T) {
	c := newCallHarness(t)
	target := filepath.Join(t.TempDir(), "existing.txt")
	_ = os.WriteFile(target, []byte("keep me"), 0o600)

	r := c.call("-a", "work", "POST", "/x", "--data", "{}", "-o", target)

	c.wantError(r, ExitUsage, "file-exists")
	if data, _ := os.ReadFile(target); string(data) != "keep me" {
		t.Errorf("the existing file was changed: %q", data)
	}
	if len(c.api.requests()) != 0 {
		t.Error("the request was sent although its response could not be saved")
	}
}

func TestCallForceReplacesAnExistingFile(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(status(200, "new body", "Content-Type", "text/plain"))
	dir := t.TempDir()
	target := filepath.Join(dir, "existing.txt")
	_ = os.WriteFile(target, []byte("old"), 0o644)

	c.mustRun("call", "-a", "work", "GET", "/x", "-o", target, "--force")

	if data, _ := os.ReadFile(target); string(data) != "new body" {
		t.Errorf("file = %q", data)
	}
	if names := dirNames(t, dir); !slices.Equal(names, []string{"existing.txt"}) {
		t.Errorf("directory = %v", names)
	}
}

func TestCallRefusesADirectoryOrAMissingParentAsOutput(t *testing.T) {
	c := newCallHarness(t)
	dir := t.TempDir()

	for name, target := range map[string]string{
		"directory":      dir,
		"missing parent": filepath.Join(dir, "nope", "file"),
	} {
		for _, extra := range [][]string{nil, {"--force"}} {
			r := c.call(append([]string{"-a", "work", "GET", "/x", "-o", target}, extra...)...)

			if r.code != ExitUsage {
				t.Errorf("%s %v: exit %d, want 2 (%s)", name, extra, r.code, r.stderr)
			}
		}
	}
	if len(c.api.requests()) != 0 {
		t.Error("a request was sent for an unusable output path")
	}
}

func TestCallWritesNoFileForAnUpstreamError(t *testing.T) {
	c := newCallHarness(t)
	const body = `{"error":{"code":404}}`
	c.api.respond(status(404, body, "Content-Type", "application/json"))
	dir := t.TempDir()

	r := c.call("-a", "work", "GET", "/x", "-o", filepath.Join(dir, "out.bin"))

	c.wantUpstream(r, ExitUpstreamClient, 404, body)
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("directory = %v, want it empty", names)
	}
}

func TestCallLeavesNoTempFileWhenTheBodyBreaksMidStream(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(func(_ int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(strings.Repeat("x", 1000)))
	})
	dir := t.TempDir()

	r := c.call("-a", "work", "GET", "/x", "-o", filepath.Join(dir, "out.bin"))

	c.wantError(r, ExitUpstreamServer, "network")
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("directory = %v, want it empty", names)
	}
}

// onWrite calls fn with every write to a stream, as a reader of a closing pipe would observe them.
type onWrite struct{ fn func() }

func (w onWrite) Write(p []byte) (int, error) { w.fn(); return len(p), nil }

func TestCallRemovesItsTemporaryNameBeforePrintingTheSummary(t *testing.T) {
	for _, force := range []bool{false, true} {
		c := newCallHarness(t)
		c.api.respond(status(200, "%PDF\x00", "Content-Type", "application/pdf"))
		dir := t.TempDir()
		args := []string{"call", "-a", "work", "GET", "/x", "-o", filepath.Join(dir, "out.pdf")}
		if force {
			args = append(args, "--force")
		}
		var namesWhenPrinted []string
		stdout := onWrite{func() { namesWhenPrinted = dirNames(t, dir) }}

		code := executeContext(t.Context(), args, stdout, &bytes.Buffer{}, c.deps)

		if code != ExitOK || !slices.Equal(namesWhenPrinted, []string{"out.pdf"}) {
			t.Errorf("force=%v: exit %d, directory while printing = %v; a killed writer must not leave a temp file", force, code, namesWhenPrinted)
		}
	}
}

func TestCallSummaryNamesThePathAsTheUserGaveIt(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(status(200, "%PDF\x00", "Content-Type", "application/pdf"))
	t.Chdir(t.TempDir())

	r := c.mustRun("call", "-a", "work", "GET", "/x", "-o", "out.pdf")

	if got := decodeJSON[savedFile](t, r); got.Path != "out.pdf" {
		t.Errorf("path = %q, want the path exactly as given", got.Path)
	}
}
