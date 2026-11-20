// Package cli wires the addn command tree and its output contract.
package cli

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/spf13/cobra"
)

// Version is overridden at build time with -ldflags "-X …/internal/cli.Version=…".
var Version = "dev"

// Run executes addn with args and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	root := newRoot()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	if err := root.Execute(); err != nil {
		failure := asError(err)
		writeErrorEnvelope(stderr, failure)
		return failure.Exit
	}
	return ExitOK
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "addn",
		Short:         "Give coding agents authenticated access to your accounts",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.AddCommand(newVersionCmd())
	return root
}

// asError keeps typed failures; anything else came from cobra's argument parsing.
func asError(err error) *Error {
	var typed *Error
	if errors.As(err, &typed) {
		return typed
	}
	return usageError(err.Error())
}

func writeErrorEnvelope(w io.Writer, e *Error) {
	envelope := map[string]map[string]string{
		"error": {"code": e.Code, "message": e.Message},
	}
	_ = json.NewEncoder(w).Encode(envelope)
}

func writeJSON(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}
