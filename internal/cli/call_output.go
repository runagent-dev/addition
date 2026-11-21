package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/runagent-dev/addition/internal/proxy"
)

// outputFile is where -o puts a 2xx body: a 0600 temp file in the target's directory, moved
// into place only once the whole body arrived. A symlink at the target is never written through.
type outputFile struct {
	path  string
	force bool
}

func badOutput(code, format string, args ...any) *Error {
	return &Error{Code: code, Exit: ExitUsage, Message: fmt.Sprintf(format, args...)}
}

// checkOutputFile validates the target before anything is sent, so a request is never made
// whose response cannot be saved.
func checkOutputFile(path string, force bool) (*outputFile, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, badOutput("bad-output", "cannot resolve the -o path")
	}
	if info, err := os.Stat(filepath.Dir(abs)); err != nil || !info.IsDir() {
		return nil, badOutput("bad-output", "the directory of %s does not exist", abs)
	}
	out := &outputFile{path: abs, force: force}
	return out, out.checkTarget()
}

func (o *outputFile) checkTarget() error {
	info, err := os.Lstat(o.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return badOutput("bad-output", "cannot inspect %s: %v", o.path, err)
	case info.Mode()&os.ModeSymlink != 0:
		return badOutput("target-is-symlink", "%s is a symbolic link; addn does not write through links", o.path)
	case !o.force:
		return badOutput("file-exists", "%s already exists; pass --force to replace it", o.path)
	case !info.Mode().IsRegular():
		return badOutput("bad-output", "%s is not a regular file", o.path)
	}
	return nil
}

type savedSummary struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Bytes       int64  `json:"bytes"`
	Path        string `json:"path"`
}

func (o *outputFile) save(cmd *cobra.Command, resp *proxy.Response, guard *proxy.EchoGuard) error {
	if guard.Leaks(resp.Header.Get("Content-Type")) {
		return credentialEcho()
	}
	size, err := o.write(resp, guard)
	if err != nil {
		return err
	}
	return writeJSON(cmd.OutOrStdout(), savedSummary{
		Status: resp.Status, ContentType: resp.Header.Get("Content-Type"), Bytes: size, Path: o.path,
	})
}

func (o *outputFile) write(resp *proxy.Response, guard *proxy.EchoGuard) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(o.path), ".addn-*.part")
	if err != nil {
		return 0, badOutput("bad-output", "cannot create a temporary file next to %s: %v", o.path, err)
	}
	defer os.Remove(tmp.Name())
	watched := &failureReader{r: guard.Reader(resp.Body)}
	size, copyErr := tmp.ReadFrom(watched)
	closeErr := tmp.Close()
	switch {
	case errors.Is(watched.err, proxy.ErrCredentialEcho):
		return 0, credentialEcho()
	case watched.err != nil:
		return 0, networkFailure()
	case copyErr != nil || closeErr != nil:
		return 0, badOutput("bad-output", "cannot write %s: %v", o.path, errors.Join(copyErr, closeErr))
	}
	return size, o.publish(tmp.Name())
}

// publish moves the finished temp file to the target: without --force it fails if the target
// appeared meanwhile; with --force it replaces a regular file but never a symlink.
func (o *outputFile) publish(tmpPath string) error {
	if o.force {
		if err := o.checkTarget(); err != nil {
			return err
		}
		return o.replace(tmpPath)
	}
	if err := os.Link(tmpPath, o.path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return badOutput("file-exists", "%s already exists; pass --force to replace it", o.path)
		}
		return o.moveWithoutLink(tmpPath)
	}
	return nil
}

func (o *outputFile) replace(tmpPath string) error {
	if err := os.Rename(tmpPath, o.path); err != nil {
		return badOutput("bad-output", "cannot replace %s: %v", o.path, err)
	}
	return nil
}

// moveWithoutLink is for file systems without hard links: check, then rename.
func (o *outputFile) moveWithoutLink(tmpPath string) error {
	if _, err := os.Lstat(o.path); err == nil {
		return badOutput("file-exists", "%s already exists; pass --force to replace it", o.path)
	}
	return o.replace(tmpPath)
}

// linkInDirectory publishes without replacing; tests swap it for a file system without hard links.
var linkInDirectory = func(dir *os.Root, oldName, newName string) error { return dir.Link(oldName, newName) }
