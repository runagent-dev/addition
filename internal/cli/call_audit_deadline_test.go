package cli

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/runagent-dev/addition/internal/vault"
)

// holdWriterLockFrom keeps the vault's writer lock on a second connection until the test ends.
func (c *callHarness) holdWriterLock() {
	c.t.Helper()
	other, err := vault.Open(c.t.Context(), c.deps.env)
	if err != nil {
		c.t.Fatal(err)
	}
	holding, stop, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_ = other.WithImmediateTx(c.t.Context(), func(*vault.Tx) error {
			close(holding)
			<-stop
			return nil
		})
		_ = other.Close()
	}()
	c.t.Cleanup(func() { close(stop); <-done })
	<-holding
}

func TestCallAuditDeadlineBoundsTheWaitForAHeldWriterLock(t *testing.T) {
	c := newCallHarness(t)
	auditTimeout = 300 * time.Millisecond
	t.Cleanup(func() { auditTimeout = 5 * time.Second })
	c.api.respond(func(_ int, w http.ResponseWriter, _ *http.Request) {
		c.holdWriterLock()
		w.WriteHeader(http.StatusNoContent)
	})

	started := time.Now()
	r := c.call("-a", "work", "POST", "/x")
	elapsed := time.Since(started)

	if r.code != ExitOK || !strings.Contains(r.stderr, `{"notice":"audit-failed"}`) {
		t.Errorf("exit %d, stderr %q; want success with the audit-failed notice", r.code, r.stderr)
	}
	if elapsed > 3*time.Second {
		t.Errorf("the call took %v; the audit deadline of 300ms must bound the wait", elapsed)
	}
}
