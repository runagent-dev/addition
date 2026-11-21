//go:build unix

package main

import (
	"syscall"
	"testing"
	"time"
)

func TestInterruptAndTerminateCancelTheCommandContext(t *testing.T) {
	for name, sig := range map[string]syscall.Signal{"SIGINT": syscall.SIGINT, "SIGTERM": syscall.SIGTERM} {
		t.Run(name, func(t *testing.T) {
			ctx, stop := signalContext()
			defer stop()

			if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
				t.Fatal(err)
			}

			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatalf("%s did not cancel the context, so interrupted writes could not clean up", name)
			}
		})
	}
}
