package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/vault"
)

// lookupProvider finds a provider by toolkit command or provider id.
func lookupProvider(c *catalog.Catalog, name string) (*catalog.Provider, bool) {
	for _, tk := range c.Toolkits() {
		if tk.Command == name {
			return c.Provider(tk.Provider)
		}
	}
	return c.Provider(name)
}

// unionScopes appends the scopes of b missing from a, keeping first-seen order.
func unionScopes(a, b []string) []string {
	out := slices.Clone(a)
	for _, s := range b {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// missingScopes lists the scopes in want that have does not hold.
func missingScopes(want, have []string) []string {
	missing := []string{}
	for _, s := range want {
		if !slices.Contains(have, s) {
			missing = append(missing, s)
		}
	}
	return missing
}

// appProvider resolves name, a provider id or toolkit command, to a connectable app.
func appProvider(c *catalog.Catalog, name string) (*catalog.Provider, error) {
	p, ok := lookupProvider(c, name)
	if !ok {
		return nil, &Error{Code: "unknown-provider", Exit: ExitUsage, Message: "unknown provider (see `addition providers`)"}
	}
	if !c.Connectable(p.ID) {
		return nil, notAnApp(c, p)
	}
	return p, nil
}

func appIDs(c *catalog.Catalog, rootID string) []string {
	var ids []string
	for _, app := range c.Apps(rootID) {
		ids = append(ids, app.ID)
	}
	return ids
}

func notAnApp(c *catalog.Catalog, p *catalog.Provider) *Error {
	apps := appIDs(c, p.IdentityRoot)
	return &Error{Code: "not-an-app", Exit: ExitUsage, Message: fmt.Sprintf(
		"%s is shared by several apps and is not one itself; use one of: %s (for example `addition connect %s --as <alias>`)",
		p.ID, strings.Join(apps, ", "), apps[0])}
}

func legacyAccountError(c *catalog.Catalog, acct vault.Account) *Error {
	example := "<app>"
	if root, err := c.IdentityRoot(acct.Provider); err == nil {
		if apps := appIDs(c, root); len(apps) > 0 {
			example = apps[0]
		}
	}
	return &Error{Code: "legacy-account", Exit: ExitNeedsConnect, Message: fmt.Sprintf(
		"%s:%s was connected before every app became its own connection; remove it with `addition accounts remove %s:%s` and connect each app you use, for example `addition connect %s --as %s`",
		acct.Provider, acct.Alias, acct.Provider, acct.Alias, example, acct.Alias)}
}

// readiness compares an account's granted scopes with its app's default scopes. An account whose
// provider is no longer an app has nothing to compare against, and a token account has no scopes to lack.
func readiness(c *catalog.Catalog, acct vault.Account, kind string) (ready bool, missing []string) {
	if kind == vault.KindAPIKey {
		return true, []string{}
	}
	p, ok := c.Provider(acct.Provider)
	if !ok || !c.Connectable(p.ID) || p.OAuth2 == nil {
		return false, []string{}
	}
	missing = missingScopes(p.OAuth2.DefaultScopes, acct.Scopes)
	return len(missing) == 0, missing
}
