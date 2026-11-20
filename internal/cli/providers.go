package cli

import "github.com/spf13/cobra"

type providerListing struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	AuthType     string  `json:"auth_type"`
	Extends      *string `json:"extends"`
	IdentityRoot string  `json:"identity_root"`
}

func newProvidersCmd(d deps) *cobra.Command {
	return &cobra.Command{
		Use:   "providers",
		Short: "List known providers as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := d.loadCatalog()
			if err != nil {
				return err
			}
			rows := []providerListing{}
			for _, p := range c.Providers() {
				root, err := c.IdentityRoot(p.ID)
				if err != nil {
					return err
				}
				row := providerListing{ID: p.ID, Name: p.Name, AuthType: p.AuthType, IdentityRoot: root}
				if p.Extends != "" {
					row.Extends = &p.Extends
				}
				rows = append(rows, row)
			}
			return writeJSON(cmd.OutOrStdout(), rows)
		},
	}
}
