package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

type toolListing struct {
	Provider    string `json:"provider"`
	Command     string `json:"command"`
	Tool        string `json:"tool"`
	CLIName     string `json:"cli_name"`
	Description string `json:"description"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Effect      string `json:"effect"`
}

func newToolsCmd() *cobra.Command {
	var provider string
	cmd := &cobra.Command{
		Use:   "tools",
		Short: "List curated tools as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := loadCatalog()
			if err != nil {
				return err
			}
			if _, ok := c.Provider(provider); provider != "" && !ok {
				return usageError(fmt.Sprintf("unknown provider %q (see `addn providers`)", provider))
			}
			rows := []toolListing{}
			for _, tk := range c.Toolkits() {
				if provider != "" && tk.Provider != provider {
					continue
				}
				for _, t := range tk.Tools {
					rows = append(rows, toolListing{
						Provider: tk.Provider, Command: tk.Command, Tool: t.ID, CLIName: t.CLIName(),
						Description: t.Description, Method: t.Method, Path: t.Path, Effect: string(t.Effect),
					})
				}
			}
			return writeJSON(cmd.OutOrStdout(), rows)
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "", "only list tools for this provider id")
	return cmd
}
