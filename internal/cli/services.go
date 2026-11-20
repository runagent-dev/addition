package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/runagent-dev/addition/internal/catalog"
)

// service is a provider under an identity root, named by its toolkit command when it has one.
type service struct {
	name     string
	provider *catalog.Provider
}

// rootServices lists the services with a toolkit that share root's identity.
func rootServices(c *catalog.Catalog, rootID string) []service {
	var out []service
	for _, tk := range c.Toolkits() {
		if r, err := c.IdentityRoot(tk.Provider); err == nil && r == rootID {
			p, _ := c.Provider(tk.Provider)
			out = append(out, service{name: tk.Command, provider: p})
		}
	}
	return out
}

// lookupService finds a service by toolkit command or provider id.
func lookupService(c *catalog.Catalog, name string) (*catalog.Provider, bool) {
	for _, tk := range c.Toolkits() {
		if tk.Command == name {
			return c.Provider(tk.Provider)
		}
	}
	return c.Provider(name)
}

// requestedScopes is the union of the default scopes of the named services, or with none
// named, of root itself and every service that has a toolkit.
func requestedScopes(c *catalog.Catalog, root *catalog.Provider, names string) ([]string, error) {
	if strings.TrimSpace(names) == "" {
		scopes := slices.Clone(root.OAuth2.DefaultScopes)
		for _, s := range rootServices(c, root.ID) {
			scopes = unionScopes(scopes, s.provider.OAuth2.DefaultScopes)
		}
		return scopes, nil
	}
	var scopes []string
	for _, name := range strings.Split(names, ",") {
		name = strings.TrimSpace(name)
		p, ok := lookupService(c, name)
		if !ok {
			return nil, &Error{Code: "unknown-service", Exit: ExitUsage,
				Message: fmt.Sprintf("unknown service %q (see `addn providers` and `addn tools`)", name)}
		}
		if r, err := c.IdentityRoot(p.ID); err != nil || r != root.ID {
			return nil, &Error{Code: "service-not-in-provider", Exit: ExitUsage,
				Message: fmt.Sprintf("service %q is not a %s service; it belongs to %s", name, root.ID, r)}
		}
		scopes = unionScopes(scopes, p.OAuth2.DefaultScopes)
	}
	return scopes, nil
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
