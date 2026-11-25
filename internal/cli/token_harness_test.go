package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

const (
	tokenApp      = "tokenapp"
	pastedToken   = "pk_live_0123456789abcdef"
	secondToken   = "pk_live_fedcba9876543210"
	tokenSubject  = "4242"
	tokenIdentity = "octo"
	createURL     = "https://tokenapp.example/settings/tokens"
)

// tokenProviderOpts vary the fake token provider's YAML.
type tokenProviderOpts struct {
	// bare sends the token with no scheme word, by an explicit empty api_key.auth_prefix.
	bare       bool
	noPrefix   bool
	noIdentity bool
}

func tokenProviderYAML(host string, o tokenProviderOpts) string {
	var b strings.Builder
	fmt.Fprintf(&b, "id: %q\nname: \"Token App\"\nauth_type: \"api_key\"\napi_key:\n  create_url: %q\n", tokenApp, createURL)
	b.WriteString("  instructions:\n    - \"Open the page.\"\n    - \"Create a token and copy it.\"\n")
	if !o.noPrefix {
		b.WriteString("  token_prefix: \"pk_\"\n")
	}
	b.WriteString("  env_var: \"TOKENAPP_TOKEN\"\n")
	if o.bare {
		b.WriteString("  auth_prefix: \"\"\n")
	}
	if !o.noIdentity {
		b.WriteString("  identity:\n    path: \"/v1/me\"\n    field: \"login\"\n    subject_field: \"id\"\n")
	}
	fmt.Fprintf(&b, "api:\n  base_url: \"https://%s\"\n  allowed_hosts:\n    - %q\n", host, host)
	b.WriteString("  auth_header: \"Authorization\"\n  auth_prefix: \"Bearer\"\n  probe_path: \"/v1/me\"\n")
	return b.String()
}

// withProviderFile returns src with providers/<id>.yaml added.
func withProviderFile(t *testing.T, src fs.FS, id, body string) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	err := fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(src, path)
		out[path] = &fstest.MapFile{Data: data}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	out["providers/"+id+".yaml"] = &fstest.MapFile{Data: []byte(body)}
	return out
}

// tokenHarness is an initialised vault with no OAuth client, and a TLS fake API that the catalog
// names as the host of the token provider "tokenapp". The API accepts the tokens in valid.
type tokenHarness struct {
	*harness
	api       *fakeAPI
	evil      *fakeAPI
	transport *recordingTransport
	opts      tokenProviderOpts

	apiMu   sync.Mutex
	valid   map[string]bool
	subject string
}

func newTokenHarness(t *testing.T, opts ...tokenProviderOpts) *tokenHarness {
	t.Helper()
	th := &tokenHarness{harness: newHarness(t), api: newFakeAPI(t), evil: newFakeAPI(t), valid: map[string]bool{pastedToken: true}, subject: tokenSubject}
	if len(opts) > 0 {
		th.opts = opts[0]
	}
	th.transport = &recordingTransport{base: th.api.srv.Client().Transport}
	th.secrets = append(th.secrets, pastedToken, secondToken)
	th.deps.catalogFS = withProviderFile(t, th.deps.catalogFS, tokenApp, tokenProviderYAML(th.api.host(), th.opts))
	th.deps.httpClient = &http.Client{Transport: th.transport, Timeout: 5 * time.Second}
	th.api.respond(th.serve)
	return th
}

// wantHeader is the Authorization value the API expects for token.
func (th *tokenHarness) wantHeader(token string) string {
	if th.opts.bare {
		return token
	}
	return "Bearer " + token
}

func (th *tokenHarness) accept(tokens ...string) {
	th.apiMu.Lock()
	defer th.apiMu.Unlock()
	th.valid = map[string]bool{}
	for _, token := range tokens {
		th.valid[token] = true
	}
}

func (th *tokenHarness) setSubject(subject string) {
	th.apiMu.Lock()
	defer th.apiMu.Unlock()
	th.subject = subject
}

// serve answers the identity endpoint; a rejected credential gets an error body that repeats what was sent.
func (th *tokenHarness) serve(_ int, w http.ResponseWriter, r *http.Request) {
	th.apiMu.Lock()
	defer th.apiMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	sent := r.Header.Get("Authorization")
	for token := range th.valid {
		if sent == th.wantHeader(token) {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": json.Number(th.subject), "login": tokenIdentity})
			return
		}
	}
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": "Bad credentials", "received": sent})
}

// connectToken connects alias with token on stdin.
func (th *tokenHarness) connectToken(alias, token string) result {
	return th.runWithStdin(strings.NewReader(token+"\n"), "connect", tokenApp, "--as", alias, "--token-stdin")
}

func (th *tokenHarness) mustConnectToken(alias, token string) {
	th.t.Helper()
	if r := th.connectToken(alias, token); r.code != ExitOK {
		th.t.Fatalf("connect: exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	th.transport.reset()
}

func (th *tokenHarness) call(args ...string) result {
	return th.run(append([]string{"call", tokenApp}, args...)...)
}

// credentialRow is what the vault stores about an account's credential, without the sealed tokens.
type credentialRow struct {
	Kind                  string
	Revision              int64
	HasRefresh, HasExpiry bool
}

func (th *tokenHarness) credentialOf(alias string) (credentialRow, bool) {
	th.t.Helper()
	var row credentialRow
	var refresh, expires *string
	err := openRawDB(th.t, th.home).QueryRow(`
		SELECT c.kind, c.revision, c.refresh_token_encrypted, c.expires_at
		FROM credentials c JOIN accounts a ON a.id = c.account_id WHERE a.provider = ? AND a.alias = ?`, tokenApp, alias).
		Scan(&row.Kind, &row.Revision, &refresh, &expires)
	if err != nil {
		return credentialRow{}, false
	}
	row.HasRefresh, row.HasExpiry = refresh != nil, expires != nil
	return row, true
}

func (th *tokenHarness) wantNothingStored() {
	th.t.Helper()
	if rows := decodeJSON[[]accountRow](th.t, th.mustRun("accounts", "list")); len(rows) != 0 {
		th.t.Errorf("accounts = %+v, want none stored", rows)
	}
	var n int
	if err := openRawDB(th.t, th.home).QueryRow(`SELECT count(*) FROM credentials`).Scan(&n); err != nil || n != 0 {
		th.t.Errorf("credentials stored = %d, %v; want none", n, err)
	}
}

// auditRows lists every audit row as "event details".
func (th *tokenHarness) auditRows() []string {
	th.t.Helper()
	rows, err := openRawDB(th.t, th.home).Query(`SELECT event || ' ' || details FROM audit_log ORDER BY id`)
	if err != nil {
		th.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var row string
		_ = rows.Scan(&row)
		out = append(out, row)
	}
	return out
}

// neverReadStdin fails the test if a command reads it, for commands that must not prompt.
type neverReadStdin struct{ t *testing.T }

func (r neverReadStdin) Read([]byte) (int, error) {
	r.t.Error("the command read standard input")
	return 0, io.EOF
}
