package cli

import "github.com/spf13/cobra"

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the addn version as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return writeJSON(cmd.OutOrStdout(), map[string]string{"version": Version})
		},
	}
}
