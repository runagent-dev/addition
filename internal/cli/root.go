// Package cli wires the addn command tree and its output contract.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// Version is overridden at build time with -ldflags "-X …/internal/cli.Version=…".
var Version = "dev"

// Run executes addn with args and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if args == nil {
		args = []string{} // cobra falls back to os.Args on nil
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if len(args) > 0 && strings.HasPrefix(args[0], cobra.ShellCompRequestCmd) {
		failure := usageError(fmt.Sprintf("unknown command %q for \"addn\"", args[0]))
		writeErrorEnvelope(stderr, failure)
		return failure.Exit
	}
	out := &trackingWriter{w: stdout}

	root := newRoot()
	root.SetArgs(args)
	root.SetOut(out)
	root.SetErr(stderr)

	err := root.Execute()
	if out.err != nil {
		err = &Error{Code: "output", Message: "writing output: " + out.err.Error(), Exit: ExitInternal}
	}
	if err != nil {
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
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetHelpCommand(newHelpCmd(root))
	root.AddCommand(newVersionCmd())
	return root
}

// newHelpCmd replaces cobra's help, which exits 0 on unknown topics.
func newHelpCmd(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:   "help [command]",
		Short: "Help about any command",
		RunE: func(_ *cobra.Command, args []string) error {
			target, rest, err := root.Find(args)
			if err != nil || len(rest) > 0 {
				return usageError(fmt.Sprintf("unknown help topic %q", strings.Join(args, " ")))
			}
			target.InitDefaultHelpFlag()
			target.InitDefaultVersionFlag()
			return target.Help()
		},
	}
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

// trackingWriter remembers the first write error, since cobra's help output discards it.
type trackingWriter struct {
	w   io.Writer
	err error
}

func (t *trackingWriter) Write(p []byte) (int, error) {
	if t.err != nil {
		return 0, t.err
	}
	n, err := t.w.Write(p)
	if err != nil {
		t.err = err
	}
	return n, err
}
