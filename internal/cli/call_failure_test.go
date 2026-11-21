package cli

import (
	"testing"

	"github.com/runagent-dev/addition/internal/proxy"
	"github.com/runagent-dev/addition/internal/vault"
)

func TestCallFailureReportsAReplacedAccountAsAUsageError(t *testing.T) {
	err := callFailure(vault.Account{Provider: "google", Alias: "work"}, proxy.ErrAccountChanged)

	failure := asError(err)
	if failure.Code != "account-changed" || failure.Exit != ExitUsage {
		t.Errorf("failure = %+v, want account-changed with exit 2", failure)
	}
}
