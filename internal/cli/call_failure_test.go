package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/proxy"
	"github.com/runagent-dev/addition/internal/vault"
)

func TestCallFailureReportsAReplacedAccountAsAUsageError(t *testing.T) {
	err := callFailure(vault.Account{Provider: "gmail", Alias: "work"}, &catalog.Provider{ID: "gmail", IdentityRoot: "google"}, proxy.ErrAccountChanged)

	failure := asError(err)
	if failure.Code != "account-changed" || failure.Exit != ExitUsage {
		t.Errorf("failure = %+v, want account-changed with exit 2", failure)
	}
}

func TestVaultBusyIsAStableLocalError(t *testing.T) {
	failure := vaultError(fmt.Errorf("open vault database: %w", vault.ErrBusy))

	if failure.Code != "vault-busy" || failure.Exit != ExitInternal || strings.Contains(strings.ToLower(failure.Message), "sqlite") {
		t.Errorf("failure = %+v, want vault-busy with exit 1 and no driver text", failure)
	}
}
