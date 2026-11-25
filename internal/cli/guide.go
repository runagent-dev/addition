package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/runagent-dev/addition/internal/guide"
)

func newGuideCmd(d deps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "guide [topic]",
		Short: "Print the built-in manual for agents, as Markdown",
		Long: "Print addition's own documentation, taken from this binary: it needs no network and reads no vault.\n\n" +
			"This is the one command that prints Markdown on stdout instead of JSON, because it is documentation.\n\n" +
			"  addition guide              the overview and the list of topics\n" +
			"  addition guide <topic>      one topic, for example connect, call, errors or gmail\n" +
			"  addition guide all          every topic in one document\n" +
			"  addition guide --json       the topics as {\"topics\":[{\"id\",\"summary\",\"command\"}]}, for programs\n\n" +
			"An unknown topic exits 2 and lists the valid ones.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if asJSON && len(args) > 0 {
				return usageError("--json lists the topics and takes no topic argument")
			}
			g, err := loadGuide(d)
			if err != nil {
				return err
			}
			return printGuide(cmd.OutOrStdout(), g, args, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "list the topics as JSON instead of printing the overview")
	return cmd
}

func loadGuide(d deps) (*guide.Guide, error) {
	c, err := d.loadCatalog()
	if err != nil {
		return nil, err
	}
	g, err := guide.Load(d.guideFS, c)
	if err != nil {
		return nil, &Error{Code: "guide", Message: err.Error(), Exit: ExitInternal}
	}
	return g, nil
}

func printGuide(w io.Writer, g *guide.Guide, args []string, asJSON bool) error {
	switch {
	case asJSON:
		return writeJSON(w, map[string]any{"topics": g.Entries()})
	case len(args) == 0:
		_, err := io.WriteString(w, g.Index(currentBuild().Version))
		return err
	case args[0] == "all":
		_, err := io.WriteString(w, g.All())
		return err
	}
	topic, ok := g.Topic(args[0])
	if !ok {
		return unknownTopic(g)
	}
	_, err := io.WriteString(w, topic.Body)
	return err
}

func unknownTopic(g *guide.Guide) *Error {
	var ids []string
	for _, e := range g.Entries() {
		ids = append(ids, e.ID)
	}
	return &Error{Code: "unknown-topic", Exit: ExitUsage,
		Message: fmt.Sprintf("unknown guide topic; valid topics: %s", strings.Join(ids, ", "))}
}
