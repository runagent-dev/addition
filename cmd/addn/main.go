package main

import (
	"context"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/runagent-dev/addition/internal/cli"
)

func main() {
	// net/http logs through the standard logger, which would put plain text, even request bytes, on stderr.
	log.SetOutput(io.Discard)
	ctx, stop := signalContext()
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// signalContext is cancelled by the first SIGINT or SIGTERM, so a running command can clean up; a second one kills the process.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}
