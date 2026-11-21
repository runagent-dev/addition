package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/vault"
)

func newClientCmd(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Manage your own OAuth clients",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newClientAddCmd(d), newClientListCmd(d))
	return cmd
}

func newClientAddCmd(d deps) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "add <provider> --file client_secret.json",
		Short: "Store an OAuth client downloaded from the provider's console",
		Long: "Store the OAuth client addition uses to connect accounts of <provider>.\n" +
			"For Google, create an OAuth client of type \"Desktop app\" in the Google Cloud console\n" +
			"and pass the JSON file it downloads. The secret is sealed in the vault; re-adding replaces it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := d.loadCatalog()
			if err != nil {
				return err
			}
			provider, err := identityRootProvider(c, args[0])
			if err != nil {
				return err
			}
			client, err := readDesktopClient(file)
			if err != nil {
				return err
			}
			client.Provider = provider.ID
			s, err := d.openVault(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			replaced := true
			err = s.WithImmediateTx(cmd.Context(), func(tx *vault.Tx) error {
				if _, err := tx.GetOAuthClient(cmd.Context(), provider.ID); errors.Is(err, vault.ErrNotFound) {
					replaced = false
				} else if err != nil {
					return err
				}
				return tx.PutOAuthClient(cmd.Context(), client)
			})
			if err != nil {
				return vaultError(err)
			}
			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"provider": provider.ID, "client_id": client.ClientID, "replaced": replaced,
			})
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "the client_secret*.json file downloaded from the provider")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

type clientFileEntry struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

func badClientFile(format string, args ...any) *Error {
	return &Error{Code: "bad-client-file", Message: fmt.Sprintf(format, args...), Exit: ExitUsage}
}

// readDesktopClient parses Google's downloaded client JSON, accepting only "Desktop app" clients.
func readDesktopClient(path string) (vault.OAuthClient, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return vault.OAuthClient{}, badClientFile("read client file: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return vault.OAuthClient{}, badClientFile("%s is not JSON", path)
	}
	if _, isWeb := doc["web"]; isWeb {
		return vault.OAuthClient{}, &Error{Code: "web-client", Exit: ExitUsage, Message: "this is a \"Web application\" client; " +
			"create an OAuth client of type \"Desktop app\" instead, which allows the loopback redirect addition uses"}
	}
	raw, ok := doc["installed"]
	if !ok {
		return vault.OAuthClient{}, badClientFile("%s has no \"installed\" client; download the JSON of a \"Desktop app\" OAuth client", path)
	}
	var entry clientFileEntry
	if err := json.Unmarshal(raw, &entry); err != nil || entry.ClientID == "" || entry.ClientSecret == "" {
		return vault.OAuthClient{}, badClientFile("%s needs installed.client_id and installed.client_secret", path)
	}
	return vault.OAuthClient{ClientID: entry.ClientID, ClientSecret: entry.ClientSecret}, nil
}

type clientListing struct {
	Provider  string    `json:"provider"`
	ClientID  string    `json:"client_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func newClientListCmd(d deps) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List stored OAuth clients as JSON, without their secrets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := d.openVault(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			clients, err := s.ListOAuthClients(cmd.Context())
			if err != nil {
				return vaultError(err)
			}
			rows := []clientListing{}
			for _, c := range clients {
				rows = append(rows, clientListing{Provider: c.Provider, ClientID: c.ClientID, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
			}
			return writeJSON(cmd.OutOrStdout(), rows)
		},
	}
}

// identityRootProvider resolves id to the OAuth provider that owns a client its apps share.
func identityRootProvider(c *catalog.Catalog, id string) (*catalog.Provider, error) {
	p, ok := c.Provider(id)
	if !ok {
		return nil, &Error{Code: "unknown-provider", Message: fmt.Sprintf("unknown provider %q (see `addition providers`)", id), Exit: ExitUsage}
	}
	root, err := c.IdentityRoot(id)
	if err != nil {
		return nil, err
	}
	if root != id {
		return nil, &Error{Code: "not-identity-root", Exit: ExitUsage,
			Message: fmt.Sprintf("%s apps share the OAuth client of %s; run `addition client add %s`", id, root, root)}
	}
	if p.AuthType != "oauth2" || p.OAuth2 == nil {
		return nil, &Error{Code: "not-oauth", Message: fmt.Sprintf("%s does not use OAuth", id), Exit: ExitUsage}
	}
	return p, nil
}
