package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/opsearch"
)

const (
	defaultSearchLimit = 8
	maxSearchLimit     = 25
)

type searchResult struct {
	Op       string   `json:"op"`
	App      string   `json:"app"`
	Summary  string   `json:"summary"`
	Effect   string   `json:"effect"`
	Method   string   `json:"method"`
	Path     string   `json:"path"`
	Required []string `json:"required"`
	Describe string   `json:"describe"`
}

type appCount struct {
	App        string         `json:"app"`
	Operations int            `json:"operations"`
	Effects    map[string]int `json:"effects"`
}

type searchOutput struct {
	Query     string         `json:"query"`
	Total     int            `json:"total"`
	Results   []searchResult `json:"results"`
	Truncated bool           `json:"truncated"`
	Narrow    string         `json:"narrow,omitempty"`
	Apps      []appCount     `json:"apps,omitempty"`
	Next      string         `json:"next"`
}

func newSearchCmd(d deps) *cobra.Command {
	var app, effect string
	limit := defaultSearchLimit
	cmd := &cobra.Command{
		Use:   "search [what you want to do]",
		Short: "Find API operations by plain-language query, as JSON",
		Long: "Search every operation the apps' own API descriptions list, offline: it reads no vault and needs no account.\n\n" +
			"  addition search send an email\n" +
			"  addition search events --app google-calendar --effect read\n" +
			"  addition search                  # no query: list the apps and how many operations each has\n\n" +
			"Each result names the operation (`op`), what it does, whether it reads, writes or deletes (`effect`), its method and\n" +
			"path, and the `describe` command that shows everything needed to call it. Run `addition describe <op>`, then `addition call`.",
		RunE: func(cmd *cobra.Command, args []string) error {
			index, err := loadOperationIndex(d)
			if err != nil {
				return err
			}
			opt, err := searchOptions(index, app, effect, limit)
			if err != nil {
				return err
			}
			query := strings.Join(args, " ")
			return writeJSON(cmd.OutOrStdout(), runSearch(index, query, opt))
		},
	}
	f := cmd.Flags()
	f.StringVar(&app, "app", "", "only operations of this app (see `addition search` with no query)")
	f.StringVar(&effect, "effect", "", "only read, write or destructive operations")
	f.IntVar(&limit, "limit", defaultSearchLimit, fmt.Sprintf("most results to return (1-%d)", maxSearchLimit))
	return cmd
}

func searchOptions(index *opsearch.Index, app, effect string, limit int) (opsearch.Options, error) {
	if limit < 1 || limit > maxSearchLimit {
		return opsearch.Options{}, usageError(fmt.Sprintf("--limit must be between 1 and %d", maxSearchLimit))
	}
	if effect != "" && !slices.Contains([]string{"read", "write", "destructive"}, effect) {
		return opsearch.Options{}, usageError(fmt.Sprintf("--effect %q must be read, write or destructive", effect))
	}
	if app != "" && !slices.ContainsFunc(index.Apps(), func(a opsearch.AppSummary) bool { return a.App == app }) {
		return opsearch.Options{}, usageError(fmt.Sprintf("unknown app %q; apps with operations: %s", app, strings.Join(appNames(index), ", ")))
	}
	return opsearch.Options{App: app, Effect: catalog.Effect(effect), Limit: limit}, nil
}

func runSearch(index *opsearch.Index, query string, opt opsearch.Options) searchOutput {
	out := searchOutput{Query: query, Results: []searchResult{}}
	if strings.TrimSpace(query) == "" {
		out.Apps = appCounts(index)
		out.Next = "addition search <what you want to do> [--app <app>]"
		return out
	}
	hits, total := index.Search(query, opt)
	out.Total = total
	for _, h := range hits {
		out.Results = append(out.Results, searchResult{
			Op: h.Op.ID, App: h.App, Summary: h.Op.Summary, Effect: string(h.Op.Effect), Method: h.Op.Method,
			Path: h.Op.Path, Required: requiredParams(h.Op), Describe: "addition describe " + h.Op.ID,
		})
	}
	out.Truncated = total > len(hits)
	switch {
	case len(hits) == 0:
		out.Apps = appCounts(index)
		out.Next = "addition search <other words> [--app <app>]"
	case out.Truncated:
		out.Narrow = "more matches exist: narrow with --app or --effect, add words, or raise --limit"
		out.Next = out.Results[0].Describe
	default:
		out.Next = out.Results[0].Describe
	}
	return out
}

// requiredParams lists the parameters a caller must supply: required and without a default.
func requiredParams(op catalog.Operation) []string {
	names := []string{}
	for _, p := range op.Params {
		if p.Required && p.Default == "" {
			names = append(names, p.Name)
		}
	}
	return names
}

func appCounts(index *opsearch.Index) []appCount {
	var out []appCount
	for _, a := range index.Apps() {
		effects := map[string]int{}
		for e, n := range a.Effects {
			effects[string(e)] = n
		}
		out = append(out, appCount{App: a.App, Operations: a.Operations, Effects: effects})
	}
	return out
}

func appNames(index *opsearch.Index) []string {
	var names []string
	for _, a := range index.Apps() {
		names = append(names, a.App)
	}
	return names
}

func loadOperationIndex(d deps) (*opsearch.Index, error) {
	c, err := d.loadCatalog()
	if err != nil {
		return nil, err
	}
	var sets []*catalog.OperationSet
	for _, id := range c.OperationProviders() {
		set, _, err := c.Operations(id)
		if err != nil {
			return nil, &Error{Code: "catalog", Message: err.Error(), Exit: ExitInternal}
		}
		sets = append(sets, set)
	}
	return opsearch.New(sets), nil
}
