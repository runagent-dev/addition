package cli

import (
	"cmp"
	"slices"

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

func newToolsCmd(d deps) *cobra.Command {
	var provider string
	cmd := &cobra.Command{
		Use:   "tools",
		Short: "List curated tools as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := d.loadCatalog()
			if err != nil {
				return err
			}
			if _, ok := c.Provider(provider); provider != "" && !ok {
				return usageError("unknown provider (see `addition providers`)")
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
			slices.SortFunc(rows, func(a, b toolListing) int {
				return cmp.Or(cmp.Compare(a.Provider, b.Provider), cmp.Compare(a.Tool, b.Tool))
			})
			return writeJSON(cmd.OutOrStdout(), rows)
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "", "only list tools for this provider id")
	return cmd
}
