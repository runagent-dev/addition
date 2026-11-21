package guide

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const rawBase = "https://raw.githubusercontent.com/runagent-dev/addition/main/"

func readLLMs(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "llms.txt"))
	if err != nil {
		t.Fatalf("llms.txt: %v", err)
	}
	return string(data)
}

func linkedPaths(t *testing.T, llms string) []string {
	t.Helper()
	var paths []string
	for _, m := range regexp.MustCompile(`\]\(([^)]+)\)`).FindAllStringSubmatch(llms, -1) {
		path, ok := strings.CutPrefix(m[1], rawBase)
		if !ok {
			t.Errorf("link %q does not point at the raw GitHub URLs of main", m[1])
			continue
		}
		paths = append(paths, path)
	}
	return paths
}

func TestLLMsLinksExist(t *testing.T) {
	paths := linkedPaths(t, readLLMs(t))
	if len(paths) == 0 {
		t.Fatal("llms.txt links nothing")
	}
	for _, p := range paths {
		if _, err := os.Stat(filepath.Join("..", "..", filepath.FromSlash(p))); err != nil {
			t.Errorf("llms.txt links %s: %v", p, err)
		}
	}
}

func TestLLMsLinksEveryGuideFile(t *testing.T) {
	llms := readLLMs(t)
	linked := strings.Join(linkedPaths(t, llms), "\n")
	guideFiles, err := filepath.Glob(filepath.Join("..", "..", "shared", "guide", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	recipes, _ := filepath.Glob(filepath.Join("..", "..", "shared", "guide", "apps", "*.md"))
	for _, f := range append(guideFiles, recipes...) {
		rel, _ := filepath.Rel(filepath.Join("..", ".."), f)
		if !strings.Contains(linked, filepath.ToSlash(rel)) {
			t.Errorf("llms.txt does not link %s", rel)
		}
	}
	if !strings.Contains(linked, "README.md") {
		t.Error("llms.txt does not link README.md")
	}
}

func TestLLMsStartsWithTheInstallAndGuide(t *testing.T) {
	llms := readLLMs(t)
	if !strings.HasPrefix(llms, "# Addition\n") {
		t.Error("llms.txt must start with the title")
	}
	for _, want := range []string{"install.sh | sh", "addition guide"} {
		if !strings.Contains(llms, want) {
			t.Errorf("llms.txt lacks %q", want)
		}
	}
}
