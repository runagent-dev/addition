package cli

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/opsearch"
)

type describeParam struct {
	Name        string   `json:"name"`
	In          string   `json:"in"`
	Type        string   `json:"type"`
	Required    bool     `json:"required"`
	Default     string   `json:"default,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Pattern     string   `json:"pattern,omitempty"`
	Description string   `json:"description,omitempty"`
}

type describeOutput struct {
	Op         string                 `json:"op"`
	App        string                 `json:"app"`
	Summary    string                 `json:"summary"`
	Method     string                 `json:"method"`
	Path       string                 `json:"path"`
	Effect     string                 `json:"effect"`
	ScopesAny  []string               `json:"scopes_any_of"`
	Params     []describeParam        `json:"params"`
	Body       *catalog.OperationBody `json:"body,omitempty"`
	UploadPath string                 `json:"upload_path,omitempty"`
	Call       string                 `json:"call"`
	Docs       string                 `json:"docs,omitempty"`
}

func newDescribeCmd(d deps) *cobra.Command {
	return &cobra.Command{
		Use:   "describe <op>",
		Short: "Show one API operation in full, with the `addition call` to run it, as JSON",
		Long: "Show everything needed to call an operation found by `addition search`: method and path, parameters, request body fields,\n" +
			"the scopes that allow it (any one of them is enough, see `scopes_any_of`) and a `call` template. Offline: no vault, no account.\n\n" +
			"  addition describe gmail.users.messages.send\n\n" +
			"An unknown id exits 2 and names the closest operations.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			index, err := loadOperationIndex(d)
			if err != nil {
				return err
			}
			entry, ok := index.Find(args[0])
			if !ok {
				return &Error{Code: "unknown-operation", Exit: ExitUsage, Message: fmt.Sprintf(
					"unknown operation %q; closest: %s; find one with `addition search <words>`",
					args[0], strings.Join(index.Nearest(args[0], 3), ", "))}
			}
			return writeJSON(cmd.OutOrStdout(), describeEntry(d, entry))
		},
	}
}

func describeEntry(d deps, e opsearch.Entry) describeOutput {
	op := e.Op
	out := describeOutput{
		Op: op.ID, App: e.App, Summary: op.Summary, Method: op.Method, Path: op.Path, Effect: string(op.Effect),
		ScopesAny: op.Scopes, Params: []describeParam{}, Body: op.Body, UploadPath: op.UploadPath,
		Call: callTemplate(e), Docs: sourceDocs(d, e.App),
	}
	if out.ScopesAny == nil {
		out.ScopesAny = []string{}
	}
	for _, p := range op.Params {
		out.Params = append(out.Params, describeParam(p))
	}
	return out
}

func sourceDocs(d deps, app string) string {
	c, err := d.loadCatalog()
	if err != nil {
		return ""
	}
	if set, ok, err := c.Operations(app); err == nil && ok {
		return set.Source.Docs
	}
	return ""
}

var camelWords = regexp.MustCompile(`([a-z0-9])([A-Z])`)

// callTemplate is the `addition call` that runs e: path values filled from defaults or $PLACEHOLDERS,
// required query parameters spelled out and a --data slot when the operation takes a body.
func callTemplate(e opsearch.Entry) string {
	op := e.Op
	path := op.Path
	for _, p := range op.Params {
		if p.In != "path" {
			continue
		}
		value := p.Default
		if value == "" {
			value = "$" + strings.ToUpper(camelWords.ReplaceAllString(p.Name, "${1}_${2}"))
		}
		path = strings.ReplaceAll(path, "{"+p.Name+"}", value)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "addition call %s -a <alias> %s %q", e.App, op.Method, path)
	for _, p := range op.Params {
		if p.In == "query" && p.Required && p.Default == "" {
			fmt.Fprintf(&b, " --query %s=VALUE", p.Name)
		}
	}
	if op.Body != nil {
		b.WriteString(" --data '<json>'")
	}
	return b.String()
}
