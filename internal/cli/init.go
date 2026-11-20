package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/runagent-dev/addition/internal/crypto"
	"github.com/runagent-dev/addition/internal/vault"
)

func newInitCmd() *cobra.Command {
	var opts vault.InitOptions
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create the local vault and its master key",
		Long: "Create the data dir, the vault database and a master key stored in the OS keychain.\n" +
			"With --key-file the key goes to $ADDN_HOME/master.key (mode 0600) instead; with\n" +
			"ADDN_MASTER_KEY set, that key is used. The vault records its source and later unlocks\n" +
			"only from it, unless ADDN_MASTER_KEY is set. Re-running never replaces a key and\n" +
			"reports the recorded key_source, even when ADDN_MASTER_KEY unlocked the vault.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := vault.Init(cmd.Context(), vault.OSEnv(), opts)
			if err != nil {
				return vaultError(err)
			}
			return writeJSON(cmd.OutOrStdout(), res)
		},
	}
	cmd.Flags().BoolVar(&opts.KeyFile, "key-file", false, "keep the master key in a 0600 file instead of the OS keychain (not on Windows)")
	return cmd
}

var vaultErrorCodes = []struct {
	err  error
	code string
}{
	{vault.ErrKeychainUnavailable, "keychain-unavailable"},
	{vault.ErrNoMasterKey, "no-master-key"},
	{vault.ErrWrongMasterKey, "wrong-master-key"},
	{crypto.ErrInvalidKey, "invalid-master-key"},
	{vault.ErrInsecureKeyFile, "insecure-key-file"},
	{vault.ErrInsecureHome, "insecure-home"},
	{vault.ErrKeyFileUnsupported, "key-file-unsupported"},
	{vault.ErrSchemaTooNew, "schema-too-new"},
	{vault.ErrNotInitialized, "not-initialized"},
	{vault.ErrRelativePath, "relative-path"},
}

// vaultError maps known setup problems to exit 2; anything else is a failure of addn itself.
func vaultError(err error) *Error {
	for _, c := range vaultErrorCodes {
		if errors.Is(err, c.err) {
			return &Error{Code: c.code, Message: err.Error(), Exit: ExitUsage}
		}
	}
	return &Error{Code: "vault", Message: err.Error(), Exit: ExitInternal}
}
