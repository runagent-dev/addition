package cli

import (
	"context"
	"io"
	"io/fs"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/vault"
	"github.com/runagent-dev/addition/shared"
)

// deps are the edges of the process that commands touch; tests swap them for fakes.
type deps struct {
	catalogFS       fs.FS
	env             vault.Env
	httpClient      *http.Client
	openBrowser     func(url string) (exited <-chan error)
	loopbackTimeout time.Duration
	// stdin replaces the process's standard input when set; sleep replaces waiting between retries.
	stdin io.Reader
	sleep func(ctx context.Context, d time.Duration) error
	// loopbackProviders is for tests: providers may list host:port API hosts and be reached over http.
	loopbackProviders bool
}

func productionDeps() deps {
	return deps{
		catalogFS:       shared.FS,
		env:             vault.OSEnv(),
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		openBrowser:     openSystemBrowser,
		loopbackTimeout: 5 * time.Minute,
	}
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
