package cli

import (
	"context"
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
	openBrowser     func(url string) error
	loopbackTimeout time.Duration
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
	c, err := catalog.Load(d.catalogFS)
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

func openSystemBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
