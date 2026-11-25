package cli

import (
	"context"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"golang.org/x/term"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/vault"
	"github.com/runagent-dev/addition/shared"
)

// deps are the edges of the process that commands touch; tests swap them for fakes.
type deps struct {
	catalogFS       fs.FS
	guideFS         fs.FS
	env             vault.Env
	httpClient      *http.Client
	openBrowser     func(url string) (exited <-chan error)
	loopbackTimeout time.Duration
	// stdin replaces the process's standard input when set; sleep replaces waiting between retries.
	stdin io.Reader
	sleep func(ctx context.Context, d time.Duration) error
	// prompter reads a secret from a person at a terminal; nil means there is none, and nothing is ever asked.
	prompter secretPrompter
	// beforeLiveToken and beforeRemoveCommit are for tests: they run just before the credential a live check
	// reads, and just before `accounts remove` takes the writer lock, so a test can change the vault in that window.
	beforeLiveToken    func()
	beforeRemoveCommit func()
	// afterSiblingSelection runs in `accounts remove --revoke` once the grant's siblings are chosen, before the revocation.
	afterSiblingSelection func()
	// loopbackProviders is for tests: providers may list host:port API hosts and be reached over http.
	loopbackProviders bool
}

func productionDeps() deps {
	return deps{
		catalogFS:       shared.FS,
		guideFS:         shared.Guide,
		env:             vault.OSEnv(),
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		openBrowser:     openSystemBrowser,
		loopbackTimeout: 5 * time.Minute,
		prompter:        terminalPrompter{fd: int(os.Stdin.Fd())},
	}
}

// secretPrompter asks a person for a secret without echoing it.
type secretPrompter interface {
	// Interactive reports whether a person is at the terminal, so asking can work.
	Interactive() bool
	ReadSecret() (string, error)
}

// terminalPrompter reads from the controlling terminal on fd.
type terminalPrompter struct{ fd int }

func (p terminalPrompter) Interactive() bool { return term.IsTerminal(p.fd) }

func (p terminalPrompter) ReadSecret() (string, error) {
	secret, err := term.ReadPassword(p.fd)
	return string(secret), err
}

// lookupEnv reads the process environment through the injected one.
func (d deps) lookupEnv(name string) (string, bool) {
	if d.env.LookupEnv == nil {
		return os.LookupEnv(name)
	}
	return d.env.LookupEnv(name)
}

func (d deps) loadCatalog() (*catalog.Catalog, error) {
	c, err := catalog.LoadWithOptions(d.catalogFS, catalog.Options{AllowHostPorts: d.loopbackProviders})
	if err != nil {
		return nil, &Error{Code: "catalog", Message: err.Error(), Exit: ExitInternal}
	}
	return c, nil
}

func (d deps) now() time.Time { return d.env.Now() }

func (d deps) openVault(ctx context.Context) (*vault.Store, error) {
	s, err := vault.Open(ctx, d.env)
	if err != nil {
		return nil, vaultError(err)
	}
	return s, nil
}

// openSystemBrowser hands url to the platform's launcher and reports, later, how it exited.
func openSystemBrowser(url string) <-chan error {
	switch runtime.GOOS {
	case "darwin":
		return launch(exec.Command("open", url))
	case "windows":
		return launch(exec.Command("rundll32", "url.dll,FileProtocolHandler", url))
	}
	return launch(exec.Command("xdg-open", url))
}

// launch starts cmd and yields, once, its start failure or its exit status.
func launch(cmd *exec.Cmd) <-chan error {
	exited := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		exited <- err
		return exited
	}
	go func() { exited <- cmd.Wait() }()
	return exited
}
