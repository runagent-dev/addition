// Package cli wires the addition command tree and its output contract.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Run executes addition with args and returns the process exit code. Cancelling ctx interrupts the command.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return executeContext(ctx, args, stdout, stderr, productionDeps())
}

func execute(args []string, stdout, stderr io.Writer, d deps) int {
	return executeContext(context.Background(), args, stdout, stderr, d)
}

func executeContext(ctx context.Context, args []string, stdout, stderr io.Writer, d deps) int {
	if d.env.Now == nil {
		d.env.Now = time.Now
	}
	if args == nil {
		args = []string{} // cobra falls back to os.Args on nil
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	out := &trackingWriter{w: stdout}

	root := newRoot(d)
	root.SetArgs(args)
	root.SetOut(out)
	root.SetErr(stderr)
	if d.stdin != nil {
		root.SetIn(d.stdin)
	}

	err := root.ExecuteContext(ctx)
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

func newRoot(d deps) *cobra.Command {
	root := &cobra.Command{
		Use:           "addition",
		Short:         "Give coding agents authenticated access to your accounts",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
		PersistentPreRunE: rejectHiddenCompletion,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetHelpCommand(newHelpCmd(root))
	root.AddCommand(
		newVersionCmd(), newProvidersCmd(d), newToolsCmd(d), newInitCmd(d),
		newClientCmd(d), newConnectCmd(d), newAccountsCmd(d), newAuthCmd(d), newCallCmd(d),
	)
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

// rejectHiddenCompletion blocks cobra's __complete command, which bypasses DisableDefaultCmd.
func rejectHiddenCompletion(cmd *cobra.Command, _ []string) error {
	if cmd.Name() == cobra.ShellCompRequestCmd {
		return usageError(fmt.Sprintf("unknown command %q for \"addition\"", cmd.CalledAs()))
	}
	return nil
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
	_ = writeJSON(w, envelope)
}

// writeJSON prints v without HTML escaping, so URLs stay copyable.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
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
