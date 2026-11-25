package cli

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/guide"
	"github.com/runagent-dev/addition/shared"
)

func loadedGuide(t *testing.T) (*guide.Guide, *catalog.Catalog) {
	t.Helper()
	c, err := catalog.Load(shared.FS)
	if err != nil {
		t.Fatal(err)
	}
	g, err := guide.Load(shared.Guide, c)
	if err != nil {
		t.Fatal(err)
	}
	return g, c
}

var fencedBlock = regexp.MustCompile("(?s)```bash\n(.*?)```")

// guideCommands returns every `addition …` line of every fenced block, with line continuations joined.
func guideCommands(body string) []string {
	var commands []string
	for _, block := range fencedBlock.FindAllStringSubmatch(body, -1) {
		joined := strings.ReplaceAll(block[1], "\\\n", " ")
		for line := range strings.SplitSeq(joined, "\n") {
			if line = strings.TrimSpace(line); strings.HasPrefix(line, "addition ") {
				commands = append(commands, line)
			}
		}
	}
	return commands
}

// shellWords splits like a POSIX shell for the quoting the guides use, stopping at a trailing comment.
func shellWords(line string) []string {
	var words []string
	var cur strings.Builder
	var quote rune
	inWord := false
	for _, r := range line {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == '#' && !inWord:
			return words
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

func isPlaceholder(s string) bool {
	return strings.HasPrefix(s, "$") || (strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">"))
}

// checkCommandLine reports what is wrong with one documented command, or "".
func checkCommandLine(root *cobra.Command, c *catalog.Catalog, topics []string, line string) string {
	words := shellWords(line)
	cmd, rest := root, words[1:]
	for len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		sub, _, err := cmd.Find(rest[:1])
		if err != nil || sub == cmd {
			break
		}
		cmd, rest = sub, rest[1:]
	}
	if cmd == root || cmd.HasSubCommands() {
		return "does not name a command"
	}
	cmd.InitDefaultHelpFlag()

	var positional []string
	for i := 0; i < len(rest); i++ {
		word := rest[i]
		if !strings.HasPrefix(word, "-") || word == "-" {
			positional = append(positional, word)
			continue
		}
		name, _, _ := strings.Cut(word, "=")
		flag := cmd.Flags().Lookup(strings.TrimLeft(name, "-"))
		if !strings.HasPrefix(name, "--") {
			flag = cmd.Flags().ShorthandLookup(strings.TrimPrefix(name, "-"))
		}
		if flag == nil {
			return "uses unknown flag " + name
		}
		if flag.Value.Type() != "bool" && !strings.Contains(word, "=") {
			i++
		}
	}
	return checkPositionals(cmd, c, topics, positional)
}

func checkPositionals(cmd *cobra.Command, c *catalog.Catalog, topics []string, positional []string) string {
	switch cmd.Name() {
	case "call":
		if len(positional) != 3 {
			return "call needs <app> <METHOD> <path>"
		}
		if !isPlaceholder(positional[0]) {
			if _, err := appProvider(c, positional[0]); err != nil {
				return "call names " + positional[0] + ", which is not a connectable app"
			}
		}
		if !isPlaceholder(positional[1]) && !isMethod(positional[1]) {
			return "call uses " + positional[1] + " as a method"
		}
	case "connect":
		if len(positional) == 1 && !isPlaceholder(positional[0]) {
			if _, err := appProvider(c, positional[0]); err != nil {
				return "connect names " + positional[0] + ", which is not a connectable app"
			}
		}
	case "guide":
		if len(positional) == 1 && !isPlaceholder(positional[0]) && !slices.Contains(topics, positional[0]) {
			return "guide names unknown topic " + positional[0]
		}
	}
	return ""
}

func TestGuideCommandsResolveAgainstTheRealCommandTree(t *testing.T) {
	g, c := loadedGuide(t)
	root := newRoot(deps{})
	var topics []string
	for _, e := range g.Entries() {
		topics = append(topics, e.ID)
	}
	checked := 0
	for _, topic := range g.Topics() {
		for _, line := range guideCommands(topic.Body) {
			checked++
			if problem := checkCommandLine(root, c, topics, line); problem != "" {
				t.Errorf("%s: %q %s", topic.ID, line, problem)
			}
		}
	}
	if checked < 30 {
		t.Errorf("only %d documented commands were checked; the guides must show real commands", checked)
	}
}

func TestCommandCheckerCatchesLies(t *testing.T) {
	_, c := loadedGuide(t)
	root := newRoot(deps{})
	lies := []string{
		"addition frobnicate",
		"addition accounts",
		"addition call gmail GET /gmail/v1/users/me/profile --nope",
		"addition call gmail GET /gmail/v1/users/me/profile --data-file x.json",
		"addition call nothing GET /x",
		"addition call gmail FETCH /x",
		"addition connect gmail --as work --headless",
		"addition guide nonsense",
	}
	for _, line := range lies {
		if checkCommandLine(root, c, []string{"call"}, line) == "" {
			t.Errorf("%q passed the checker", line)
		}
	}
	truths := []string{
		"addition call gmail -a work GET /gmail/v1/users/me/profile --query 'q=a b' -H 'X-A: b' -o out.json",
		"addition call <app> GET <path>",
		"addition connect --complete '<url>'",
		"addition accounts remove gmail:work --revoke --and-siblings",
	}
	for _, line := range truths {
		if problem := checkCommandLine(root, c, nil, line); problem != "" {
			t.Errorf("%q: %s", line, problem)
		}
	}
}

var (
	codeLiteral = regexp.MustCompile(`(?:Code:|\bcode :?=) *"([a-z][a-z0-9-]*)"`)
	badOutputs  = regexp.MustCompile(`badOutput\("([a-z][a-z0-9-]*)"`)
	tableCodes  = regexp.MustCompile(`(?:\{\w+\.Err\w+, |\boauth\.Op\w+: +)"([a-z][a-z0-9-]*)"`)
)

// emittedErrorCodes scans the non-test Go sources for every code an Error or RequestError can carry.
func emittedErrorCodes(t *testing.T) []string {
	t.Helper()
	found := map[string]bool{}
	err := filepath.WalkDir(filepath.Join("..", "..", "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		patterns := []*regexp.Regexp{codeLiteral, badOutputs, tableCodes}
		if filepath.Base(path) == "doctor.go" {
			patterns = patterns[:2] // its tables name check ids, not error codes
		}
		for _, re := range patterns {
			for _, m := range re.FindAllStringSubmatch(string(src), -1) {
				found[m[1]] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	codes := make([]string, 0, len(found))
	for code := range found {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	return codes
}

var (
	errorsTableRow = regexp.MustCompile("(?m)^\\| ((?:`[a-z0-9-]+`(?:, )?)+) \\| [1-5]")
	backticked     = regexp.MustCompile("`([a-z0-9-]+)`")
)

func TestErrorsTopicListsExactlyTheCodesTheCodeEmits(t *testing.T) {
	g, _ := loadedGuide(t)
	topic, _ := g.Topic("errors")
	documented := map[string]bool{}
	for _, row := range errorsTableRow.FindAllStringSubmatch(topic.Body, -1) {
		for _, code := range backticked.FindAllStringSubmatch(row[1], -1) {
			documented[code[1]] = true
		}
	}
	emitted := emittedErrorCodes(t)
	if len(emitted) < 40 {
		t.Fatalf("found only %d codes in the source; the scan is broken: %v", len(emitted), emitted)
	}
	for _, code := range emitted {
		if !documented[code] {
			t.Errorf("error code %q can be emitted but errors.md does not document it", code)
		}
		delete(documented, code)
	}
	for code := range documented {
		t.Errorf("errors.md documents %q, which no code emits", code)
	}
}

var scopeURL = regexp.MustCompile(`https://www\.googleapis\.com/auth/[a-z._]+`)

func TestRecipesMatchTheirProviders(t *testing.T) {
	g, c := loadedGuide(t)
	recipes := 0
	for _, topic := range g.Topics() {
		p, ok := c.Provider(topic.ID)
		if !ok {
			continue
		}
		recipes++
		if !c.Connectable(p.ID) {
			t.Errorf("recipe %s is not a connectable app", p.ID)
		}
		if p.OAuth2 == nil {
			continue
		}
		for _, scope := range scopeURL.FindAllString(topic.Body, -1) {
			if !slices.Contains(p.OAuth2.DefaultScopes, scope) {
				t.Errorf("recipe %s names scope %s, which %s does not request", p.ID, scope, p.ID)
			}
		}
		for _, scope := range p.OAuth2.DefaultScopes {
			if strings.HasPrefix(scope, "https://") && !strings.Contains(topic.Body, scope) {
				t.Errorf("recipe %s does not name its scope %s", p.ID, scope)
			}
		}
	}
	if recipes != 6 {
		t.Errorf("found %d recipes, want the five Google apps and ClickUp", recipes)
	}
}

func TestGuideFilesAreWellFormed(t *testing.T) {
	g, _ := loadedGuide(t)
	for _, topic := range g.Topics() {
		first, _, _ := strings.Cut(topic.Body, "\n")
		if strings.TrimSpace(first) == "" || strings.HasPrefix(first, "#") {
			t.Errorf("%s: the first line must be a one-line summary, got %q", topic.ID, first)
		}
		for _, banned := range []string{"TODO", "TBD", "FIXME", "addn"} {
			if strings.Contains(topic.Body, banned) {
				t.Errorf("%s contains %q", topic.ID, banned)
			}
		}
	}
}

func TestGuideDoesNotAdvertiseUnshippedFeatures(t *testing.T) {
	g, _ := loadedGuide(t)
	for _, topic := range g.Topics() {
		body := topic.Body
		if i := strings.Index(body, "## Coming"); i >= 0 {
			body = body[:i]
		}
		for _, planned := range []string{"search-messages", "get-attachment", "drive upload", "skill install"} {
			if strings.Contains(body, planned) {
				t.Errorf("%s mentions %q outside a Coming list", topic.ID, planned)
			}
		}
	}
}

var bashBlock = regexp.MustCompile("(?s)```bash\n(.*?)```")

func bashBlocks(body string) []string {
	var blocks []string
	for _, m := range bashBlock.FindAllStringSubmatch(body, -1) {
		blocks = append(blocks, m[1])
	}
	return blocks
}

func TestGuideBashBlocksParse(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	g, _ := loadedGuide(t)
	for _, topic := range g.Topics() {
		for i, block := range bashBlocks(topic.Body) {
			cmd := exec.Command(bash, "-n")
			cmd.Stdin = strings.NewReader(block)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s: bash block %d does not parse: %v\n%s\n%s", topic.ID, i+1, err, out, block)
			}
		}
	}
}

var (
	quotedSpan       = regexp.MustCompile(`'[^']*'|"[^"]*"`)
	trailingComment  = regexp.MustCompile(`(^|\s)#.*$`)
	anglePlaceholder = regexp.MustCompile(`<[A-Za-z]`)
)

func TestGuideBashPlaceholdersAreQuotedOrVariables(t *testing.T) {
	g, _ := loadedGuide(t)
	for _, topic := range g.Topics() {
		for _, block := range bashBlocks(topic.Body) {
			for line := range strings.SplitSeq(block, "\n") {
				bare := trailingComment.ReplaceAllString(quotedSpan.ReplaceAllString(line, "''"), "")
				if anglePlaceholder.MatchString(bare) {
					t.Errorf("%s: unquoted <placeholder> in %q; use a quoted \"$VARIABLE\"", topic.ID, line)
				}
			}
		}
	}
}

func TestPaginatedRecipesRepeatTheirQueryAndAskForTheToken(t *testing.T) {
	g, _ := loadedGuide(t)
	pages := 0
	for _, topic := range g.Topics() {
		for _, block := range bashBlocks(topic.Body) {
			for line := range strings.SplitSeq(block, "\n") {
				if !strings.Contains(line, "pageToken=") {
					continue
				}
				pages++
				if strings.Contains(line, "fields=") && !strings.Contains(line, "nextPageToken") {
					t.Errorf("%s: a paginated call whose fields mask drops nextPageToken: %q", topic.ID, line)
				}
				if topic.ID == "gmail" && !strings.Contains(line, "q=") {
					t.Errorf("gmail: the next page must repeat the search query: %q", line)
				}
			}
		}
	}
	if pages < 3 {
		t.Errorf("found %d paginated examples, want one per list-style app", pages)
	}
}

func TestAccessTokenStatesAreDocumentedFromTheCode(t *testing.T) {
	src, err := os.ReadFile("check.go")
	if err != nil {
		t.Fatal(err)
	}
	_, fn, _ := strings.Cut(string(src), "func accessTokenState")
	fn, _, _ = strings.Cut(fn, "\n}\n")
	states := regexp.MustCompile(`return "([a-z-]+)"`).FindAllStringSubmatch(fn, -1)
	if len(states) < 5 {
		t.Fatalf("found %d states in accessTokenState; the scan is broken", len(states))
	}
	g, _ := loadedGuide(t)
	accounts, _ := g.Topic("accounts")
	for _, m := range states {
		if !strings.Contains(accounts.Body, "`"+m[1]+"`") {
			t.Errorf("accounts.md does not document access_token state %q", m[1])
		}
	}
}

func TestGuideStatesTheContractsReviewersFoundMissing(t *testing.T) {
	required := map[string][]string{
		"overview":     {"addition init --key-file", "ADDITION_MASTER_KEY", "keychain-unavailable", "on_path", "Windows", "field level"},
		"connect":      {"read -r", `"$URL"`, "history", "one-use"},
		"accounts":     {"created_at", "updated_at", "stored state", "live", "legacy-account", "same Cloud project", "local guard"},
		"call":         {"unsupported-encoding", "1 MiB", "empty", "content_type", "raw"},
		"google-chat":  {"SERVICE_DISABLED", "app configuration", "nextPageToken", "any page size", "pageSize=50"},
		"google-drive": {"about", "explicit"},
	}
	g, _ := loadedGuide(t)
	for id, phrases := range required {
		topic, _ := g.Topic(id)
		for _, phrase := range phrases {
			if !strings.Contains(topic.Body, phrase) {
				t.Errorf("%s lacks %q", id, phrase)
			}
		}
	}
	overview, _ := g.Topic("overview")
	for _, wrong := range []string{"encrypted SQLite file", "Commands print JSON on stdout."} {
		if strings.Contains(overview.Body, wrong) {
			t.Errorf("overview still says %q", wrong)
		}
	}
}
