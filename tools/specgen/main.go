// Command specgen writes shared/operations/<provider>.yaml from the vendors' own API descriptions.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/specimport"
	"github.com/runagent-dev/addition/shared"
)

//go:embed sources.yaml
var sourcesYAML []byte

const maxDescriptionBytes = 32 << 20

type source struct {
	Provider string `yaml:"provider"`
	Kind     string `yaml:"kind"`
	URL      string `yaml:"url"`
}

func main() {
	out := flag.String("out", "shared/operations", "directory to write <provider>.yaml files into")
	from := flag.String("from", "", "read <provider>.json from this directory instead of fetching")
	only := flag.String("only", "", "generate only this provider")
	flag.Parse()
	if err := run(*out, *from, *only); err != nil {
		fmt.Fprintln(os.Stderr, "specgen:", err)
		os.Exit(1)
	}
}

func run(out, from, only string) error {
	var manifest struct {
		Sources []source `yaml:"sources"`
	}
	if err := yaml.Unmarshal(sourcesYAML, &manifest); err != nil {
		return err
	}
	cat, err := catalog.Load(shared.FS)
	if err != nil {
		return err
	}
	for _, s := range manifest.Sources {
		if only != "" && s.Provider != only {
			continue
		}
		provider, ok := cat.Provider(s.Provider)
		if !ok {
			return fmt.Errorf("%s: unknown provider", s.Provider)
		}
		data, err := read(s, from)
		if err != nil {
			return fmt.Errorf("%s: %w", s.Provider, err)
		}
		set, err := generate(s, data)
		if err != nil {
			return fmt.Errorf("%s: %w", s.Provider, err)
		}
		if err := catalog.ValidateOperationSet(set, provider); err != nil {
			return fmt.Errorf("%s: %w", s.Provider, err)
		}
		yamlBytes, err := specimport.Marshal(set)
		if err != nil {
			return err
		}
		file := filepath.Join(out, s.Provider+".yaml")
		if err := os.WriteFile(file, yamlBytes, 0o644); err != nil {
			return err
		}
		fmt.Printf("%s: %d operations (%s revision %s)\n", file, len(set.Operations), set.Source.ID, set.Source.Revision)
	}
	return nil
}

func generate(s source, data []byte) (*catalog.OperationSet, error) {
	if s.Kind != "google-discovery" {
		return nil, fmt.Errorf("unknown source kind %q", s.Kind)
	}
	return specimport.GoogleDiscovery(s.Provider, data)
}

func read(s source, from string) ([]byte, error) {
	if from != "" {
		return os.ReadFile(filepath.Join(from, s.Provider+".json"))
	}
	u, err := url.Parse(s.URL)
	if err != nil || u.Scheme != "https" {
		return nil, fmt.Errorf("source url %q must be https", s.URL)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(s.URL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", s.URL, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDescriptionBytes))
}
