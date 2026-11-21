package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root can write into any directory")
	}
}

func TestCallRefusesAnUnwritableOutputDirectoryBeforeSending(t *testing.T) {
	skipIfRoot(t)
	c := newCallHarness(t)
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o555); err != nil {
		t.Fatal(err)
	}

	r := c.call("-a", "work", "POST", "/x", "--data", "{}", "-o", filepath.Join(dir, "out"))

	c.wantError(r, ExitUsage, "bad-output")
	if n := len(c.api.requests()); n != 0 {
		t.Errorf("the API saw %d requests; a mutation must not be sent when its response cannot be saved", n)
	}
}

func TestCallCreatesTheTempFileBeforeSendingAndRemovesItAfterAnUpstreamError(t *testing.T) {
	c := newCallHarness(t)
	dir := t.TempDir()
	var during []string
	c.api.respond(func(n int, w http.ResponseWriter, r *http.Request) {
		during = dirNames(t, dir)
		status(404, `{"error":"missing"}`, "Content-Type", "application/json")(n, w, r)
	})

	r := c.call("-a", "work", "GET", "/x", "-o", filepath.Join(dir, "out"))

	c.wantUpstream(r, ExitUpstreamClient, 404, `{"error":"missing"}`)
	if len(during) != 1 || !strings.HasPrefix(during[0], ".addn-") || !strings.HasSuffix(during[0], ".part") {
		t.Errorf("directory while the request was in flight = %v, want one private temp file", during)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("directory afterwards = %v, want it empty", names)
	}
}

func TestCallWithoutForceNeverReplacesAFileThatAppearsDuringTheCall(t *testing.T) {
	c := newCallHarness(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "out")
	c.api.respond(func(n int, w http.ResponseWriter, r *http.Request) {
		_ = os.WriteFile(target, []byte("someone else's"), 0o600)
		status(200, "new body", "Content-Type", "text/plain")(n, w, r)
	})

	r := c.call("-a", "work", "GET", "/x", "-o", target)

	c.wantError(r, ExitUsage, "file-exists")
	if data, _ := os.ReadFile(target); string(data) != "someone else's" {
		t.Errorf("target = %q, want the other writer's file untouched", data)
	}
	if names := dirNames(t, dir); !slices.Equal(names, []string{"out"}) {
		t.Errorf("directory = %v, want only the target", names)
	}
}

func TestCallWithoutForceFailsBeforeSendingWhereHardLinksAreUnavailable(t *testing.T) {
	c := newCallHarness(t)
	defer func(prev func(*os.Root, string, string) error) { linkInDirectory = prev }(linkInDirectory)
	linkInDirectory = func(*os.Root, string, string) error { return &os.LinkError{Op: "link", Err: syscall.EPERM} }
	dir := t.TempDir()

	r := c.call("-a", "work", "POST", "/x", "--data", "{}", "-o", filepath.Join(dir, "out"))

	env := c.wantError(r, ExitUsage, "bad-output")
	if !strings.Contains(env.Error.Message, "--force") {
		t.Errorf("message %q does not say --force is the way out", env.Error.Message)
	}
	if len(c.api.requests()) != 0 {
		t.Error("a request was sent although the file could not be published without replacing")
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("directory = %v, want it empty", names)
	}

	c.mustRun("call", "gmail", "-a", "work", "GET", "/x", "-o", filepath.Join(dir, "forced"), "--force")
	if names := dirNames(t, dir); !slices.Equal(names, []string{"forced"}) {
		t.Errorf("--force needs no hard link, directory = %v", names)
	}
}

func TestCallForceNeverWritesThroughASymlinkThatAppearsDuringTheCall(t *testing.T) {
	c := newCallHarness(t)
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	_ = os.WriteFile(victim, []byte("precious"), 0o600)
	target := filepath.Join(dir, "out")
	_ = os.WriteFile(target, []byte("old"), 0o600)
	c.api.respond(func(n int, w http.ResponseWriter, r *http.Request) {
		_ = os.Remove(target)
		_ = os.Symlink(victim, target)
		status(200, "new body", "Content-Type", "text/plain")(n, w, r)
	})

	r := c.call("-a", "work", "GET", "/x", "-o", target, "--force")

	c.wantError(r, ExitUsage, "target-is-symlink")
	if data, _ := os.ReadFile(victim); string(data) != "precious" {
		t.Errorf("the symlink's target was written: %q", data)
	}
	for _, name := range dirNames(t, dir) {
		if strings.HasSuffix(name, ".part") {
			t.Errorf("temp file %s left behind", name)
		}
	}
}

func TestCallPublishesIntoTheDirectoryItPinnedWhenThePathIsRenamedMidStream(t *testing.T) {
	for name, truncate := range map[string]bool{"complete body": false, "truncated body": true} {
		t.Run(name, func(t *testing.T) {
			c := newCallHarness(t)
			parent := t.TempDir()
			dir := filepath.Join(parent, "dest")
			moved := filepath.Join(parent, "moved")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			first, second := strings.Repeat("a", 4096), strings.Repeat("b", 4096)
			c.api.respond(func(_ int, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				if truncate {
					w.Header().Set("Content-Length", "100000")
				}
				_, _ = w.Write([]byte(first))
				w.(http.Flusher).Flush()
				if err := os.Rename(dir, moved); err != nil {
					t.Error(err)
				}
				_, _ = w.Write([]byte(second))
			})

			r := c.call("-a", "work", "GET", "/x", "-o", filepath.Join(dir, "out.bin"))

			if truncate {
				c.wantError(r, ExitUpstreamServer, "network")
				if names := dirNames(t, moved); len(names) != 0 {
					t.Errorf("moved directory = %v, want the partial file cleaned up there", names)
				}
				return
			}
			if r.code != ExitOK {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}
			data, _ := os.ReadFile(filepath.Join(moved, "out.bin"))
			if !bytes.Equal(data, []byte(first+second)) {
				t.Errorf("moved/out.bin = %d bytes, want the whole body published in the directory that was opened", len(data))
			}
			if names := dirNames(t, moved); !slices.Equal(names, []string{"out.bin"}) {
				t.Errorf("moved directory = %v", names)
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the original path was recreated (err = %v)", err)
			}
		})
	}
}

func TestCallCleansUpAPartialFileWhenInterruptedMidStream(t *testing.T) {
	c := newCallHarness(t)
	dir := t.TempDir()
	streaming, stop := make(chan struct{}), make(chan struct{})
	c.api.respond(func(_ int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "100000")
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
		w.(http.Flusher).Flush()
		close(streaming)
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	})
	t.Cleanup(func() { close(stop) })
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-streaming
		cancel()
	}()

	r := c.runContext(ctx, nil, "call", "gmail", "-a", "work", "GET", "/x", "-o", filepath.Join(dir, "out.bin"))

	c.wantError(r, ExitInternal, "interrupted")
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("directory = %v, want the partial file removed on interruption", names)
	}
}
