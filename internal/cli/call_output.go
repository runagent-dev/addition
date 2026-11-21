package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/runagent-dev/addition/internal/proxy"
)

// outputFile is where -o puts a 2xx body. It opens the target's directory once and keeps a 0600 temp
// file there, created before anything is sent; the finished body is published into that same
// directory without replacing anything unless --force. A symlink at the target is never written through.
type outputFile struct {
	path     string
	given    string // the -o argument as the user wrote it, which the summary reports
	name     string
	force    bool
	dir      *os.Root
	temp     *os.File
	tempName string
}

func badOutput(code, format string, args ...any) *Error {
	return &Error{Code: code, Exit: ExitUsage, Message: fmt.Sprintf(format, args...)}
}

// linkInDirectory publishes without replacing; tests swap it for a file system without hard links.
var linkInDirectory = func(dir *os.Root, oldName, newName string) error { return dir.Link(oldName, newName) }

// openOutputFile prepares the target before anything is sent, so a request is never made whose
// response cannot be saved. The caller must discard the result.
func openOutputFile(path string, force bool) (*outputFile, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, badOutput("bad-output", "cannot resolve the -o path")
	}
	name := filepath.Base(abs)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return nil, badOutput("bad-output", "%s is not a file name", abs)
	}
	dir, err := os.OpenRoot(filepath.Dir(abs))
	if err != nil {
		return nil, badOutput("bad-output", "cannot open the directory of %s: %v", abs, err)
	}
	out := &outputFile{path: abs, given: path, name: name, force: force, dir: dir}
	if err := out.prepare(); err != nil {
		out.discard()
		return nil, err
	}
	return out, nil
}

func (o *outputFile) prepare() error {
	if err := o.checkTarget(); err != nil {
		return err
	}
	if err := o.createTemp(); err != nil {
		return err
	}
	if o.force {
		return nil
	}
	return o.checkLinking()
}

func (o *outputFile) checkTarget() error {
	info, err := o.dir.Lstat(o.name)
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

func (o *outputFile) createTemp() error {
	for range 10 {
		name := ".addn-" + randomSuffix() + ".part"
		file, err := o.dir.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return badOutput("bad-output", "cannot create a temporary file next to %s: %v", o.path, err)
		}
		o.temp, o.tempName = file, name
		return nil
	}
	return badOutput("bad-output", "cannot create a temporary file next to %s", o.path)
}

func randomSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// checkLinking proves the directory can publish without replacing, since that is the only way
// to keep a file that appears meanwhile; --force needs no such proof.
func (o *outputFile) checkLinking() error {
	probe := o.tempName + ".probe"
	if err := linkInDirectory(o.dir, o.tempName, probe); err != nil {
		return badOutput("bad-output", "the file system of %s cannot publish a file without replacing one; pass --force", o.path)
	}
	_ = o.dir.Remove(probe)
	return nil
}

// discard closes the output and removes whatever temp file is left; it is safe to call more than once.
func (o *outputFile) discard() {
	if o == nil {
		return
	}
	if o.temp != nil {
		_ = o.temp.Close()
		o.temp = nil
	}
	if o.tempName != "" {
		_ = o.dir.Remove(o.tempName)
		o.tempName = ""
	}
	if o.dir != nil {
		_ = o.dir.Close()
		o.dir = nil
	}
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
	size, err := o.write(cmd, resp, guard)
	if err != nil {
		return err
	}
	return writeJSON(cmd.OutOrStdout(), savedSummary{
		Status: resp.Status, ContentType: resp.Header.Get("Content-Type"), Bytes: size, Path: o.given,
	})
}

func (o *outputFile) write(cmd *cobra.Command, resp *proxy.Response, guard *proxy.EchoGuard) (int64, error) {
	watched := &failureReader{r: guard.Reader(resp.Body)}
	size, copyErr := o.temp.ReadFrom(watched)
	closeErr := o.temp.Close()
	o.temp = nil
	switch {
	case watched.err != nil:
		return 0, readFailure(cmd, watched.err)
	case copyErr != nil || closeErr != nil:
		return 0, badOutput("bad-output", "cannot write %s: %v", o.path, errors.Join(copyErr, closeErr))
	}
	return size, o.publish()
}

// publish moves the finished temp file to the target: without --force it fails if the target
// exists; with --force it replaces a regular file but never a symlink.
func (o *outputFile) publish() error {
	if o.force {
		return o.replace()
	}
	if err := linkInDirectory(o.dir, o.tempName, o.name); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return badOutput("file-exists", "%s already exists; pass --force to replace it", o.path)
		}
		return badOutput("bad-output", "cannot publish %s: %v", o.path, err)
	}
	_ = o.dir.Remove(o.tempName)
	o.tempName = ""
	return nil
}

func (o *outputFile) replace() error {
	if err := o.checkTarget(); err != nil {
		return err
	}
	if err := o.dir.Rename(o.tempName, o.name); err != nil {
		return badOutput("bad-output", "cannot replace %s: %v", o.path, err)
	}
	o.tempName = ""
	return nil
}
