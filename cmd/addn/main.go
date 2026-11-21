package main

import (
	"context"
	"io"
	"log"
	"os"

	"github.com/runagent-dev/addition/internal/cli"
)

func main() {
	// net/http logs through the standard logger, which would put plain text, even request bytes, on stderr.
	log.SetOutput(io.Discard)
	os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
