package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/crypto"
	"github.com/runagent-dev/addition/internal/vault"
)

const (
	checkOK   = "ok"
	checkWarn = "warn"
	checkFail = "fail"
	checkSkip = "skip"
)

const (
	codeLegacy         = "legacy"
	codeNeedsReauth    = "needs-reauth"
	codeNoCredential   = "no-credential"
	codeNoRefreshToken = "no-refresh-token"
	codeScopesMissing  = "scopes-missing"
)

const (
	fixInitVault = "addition init"
	fixHome      = "set ADDITION_HOME to an absolute path"
)

type doctorCheck struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Code    string `json:"code,omitempty"`
	Summary string `json:"summary"`
	Fix     string `json:"fix,omitempty"`
	Hint    string `json:"hint,omitempty"`
	// actionable marks a warning the user has to act on, which keeps the report from being ok.
	actionable bool
}

func (c doctorCheck) blocking() bool { return c.Status == checkFail || c.actionable }

type doctorReport struct {
	OK      bool          `json:"ok"`
	Version string        `json:"version"`
	Home    string        `json:"home"`
	Checks  []doctorCheck `json:"checks"`
	Next    *string       `json:"next"`
}

func newDoctorCmd(d deps) *cobra.Command {
	var live bool
	cmd := &cobra.Command{
		Use:   "doctor [--live]",
		Short: "Diagnose the install and say what to run next, as JSON",
		Long: "Check the vault, its master key, the OAuth clients and every connected account, and print one\n" +
			"JSON report: ok, version, home, checks (each with an id, a status of ok, warn, fail or skip, a\n" +
			"summary, and a fix or hint when there is one) and next, the one command to run now (null when\n" +
			"all is well).\n\n" +
			"doctor opens the vault like any other command (it may apply a pending schema upgrade) and makes no\n" +
			"network request unless you pass --live. It never creates a data dir or vault that is missing.\n\n" +
			"With --live, each connected app also gets one cheap authenticated GET through the same path as\n" +
			"`addition call`, so a disabled API or a revoked grant shows up as live:<app>:<alias>. A refresh\n" +
			"and the probe are recorded like any other call. A probe takes up to 15 s per request, plus at most the\n" +
			"token endpoint's own timeout when it has to refresh a token.\n\n" +
			"Exit codes: 0 when ok; 3 when something needs you to act (the report is still on stdout);\n" +
			"1 when addition itself failed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runDoctor(cmd, d, live) },
	}
	cmd.Flags().BoolVar(&live, "live", false, "also probe each connected app's API once")
	return cmd
}

// runDoctor opens the vault like any other command and reports one snapshot of it: everything it says
// about clients, accounts and the key source comes from a single read transaction.
func runDoctor(cmd *cobra.Command, d deps, live bool) error {
	ctx := cmd.Context()
	cat, err := d.loadCatalog()
	if err != nil {
		return err
	}
	rep := doctorReport{Version: currentBuild().Version, Checks: []doctorCheck{}}
	if rep.Home, err = vault.ResolveHome(d.env); err != nil {
		rep.Checks = append(rep.Checks, doctorCheck{ID: "vault", Status: checkFail, Summary: "the data dir cannot be located", Fix: fixHome})
		return finishDoctor(cmd, rep)
	}
	if missing := databaseChecks(rep.Home); missing != nil {
		rep.Checks = append(rep.Checks, missing...)
		return finishDoctor(cmd, rep)
	}
	store, err := vault.Open(ctx, d.env)
	if err != nil {
		failed, ok := openFailureChecks(rep.Home, err)
		if !ok {
			return vaultError(err)
		}
		rep.Checks = append(rep.Checks, failed...)
		return finishDoctor(cmd, rep)
	}
	defer store.Close()
	snap, err := store.Snapshot(ctx)
	if err != nil {
		return vaultError(err)
	}

	session := &accountSession{catalog: cat, store: store}
	clientIDs := map[string]string{}
	for _, c := range snap.Clients {
		clientIDs[c.Provider] = c.ClientID
	}
	accounts := session.accountChecks(snap, d.now())
	rep.Checks = append(rep.Checks,
		doctorCheck{ID: "vault", Status: checkOK, Summary: fmt.Sprintf("the vault is initialised (schema %d)", vault.SchemaVersion)},
		doctorCheck{ID: "master-key", Status: checkOK, Summary: fmt.Sprintf("the master key unlocks the vault (recorded source: %s)", snap.KeySource)},
	)
	rep.Checks = append(rep.Checks, session.clientChecks(clientIDs, snap)...)
	rep.Checks = append(rep.Checks, accounts.checks...)
	rep.Checks = append(rep.Checks, session.cloudSetupChecks(snap, clientIDs)...)
	if live {
		probes, err := session.liveChecks(cmd, d, accounts)
		if err != nil {
			return err
		}
		rep.Checks = append(rep.Checks, probes...)
	}
	return finishDoctor(cmd, rep)
}

const databaseFile = "addition.db"

// databaseChecks stats the vault file without opening it, so a missing data dir or database creates nothing.
// It returns nil when the file is there.
func databaseChecks(home string) []doctorCheck {
	_, err := os.Stat(filepath.Join(home, databaseFile))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return []doctorCheck{
			{ID: "vault", Status: checkFail, Summary: "there is no vault in the data dir yet", Fix: fixInitVault},
			{ID: "master-key", Status: checkSkip, Summary: "there is no usable vault to unlock"},
		}
	}
	return []doctorCheck{
		{ID: "vault", Status: checkFail, Summary: "the data dir cannot be read by this user", Fix: "make the data dir readable by your user only (chmod 700)"},
		{ID: "master-key", Status: checkSkip, Summary: "there is no readable vault to unlock"},
	}
}

// finishDoctor decides ok and next from the checks, prints the report and, unless ok, fails with exit 3.
func finishDoctor(cmd *cobra.Command, rep doctorReport) error {
	rep.OK = true
	for _, c := range rep.Checks {
		if !c.blocking() {
			continue
		}
		rep.OK = false
		if rep.Next == nil && c.Fix != "" {
			rep.Next = &c.Fix
		}
	}
	if err := writeJSON(cmd.OutOrStdout(), rep); err != nil {
		return err
	}
	if rep.OK {
		return nil
	}
	message := "addition found problems; see checks in the report"
	if rep.Next != nil {
		message = "addition found problems; run first: " + *rep.Next
	}
	return &Error{Code: "needs-action", Message: message, Exit: ExitNeedsConnect}
}

// openFailureChecks explains why the vault could not be opened; ok is false for a failure of addition itself.
func openFailureChecks(home string, err error) (checks []doctorCheck, ok bool) {
	if file, named := errors.AsType[*vault.InsecureFileError](err); named && errors.Is(err, vault.ErrInsecureHome) {
		return []doctorCheck{insecureFileCheck(home, file), {ID: "master-key", Status: checkSkip, Summary: "there is no usable vault to unlock"}}, true
	}
	for _, f := range openFailures {
		if !errors.Is(err, f.err) {
			continue
		}
		failed := doctorCheck{ID: f.check, Status: checkFail, Summary: f.summary, Fix: f.fix}
		if f.check != "vault" {
			return []doctorCheck{{ID: "vault", Status: checkOK, Summary: "the vault database is there"}, failed}, true
		}
		return []doctorCheck{failed, {ID: "master-key", Status: checkSkip, Summary: "there is no usable vault to unlock"}}, true
	}
	return nil, false
}

// insecureFileCheck names the file the vault refused and the command that fixes it, not the data dir.
func insecureFileCheck(home string, file *vault.InsecureFileError) doctorCheck {
	path := shellQuote(filepath.Join(home, file.Name))
	if file.TooPermissive {
		return doctorCheck{ID: "vault", Status: checkFail, Summary: file.Name + " is readable by other users", Fix: "chmod 600 " + path}
	}
	return doctorCheck{ID: "vault", Status: checkFail, Summary: file.Name + " is not a regular file owned by this user", Fix: "move " + path + " aside"}
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func shellQuote(path string) string {
	if shellSafe.MatchString(path) {
		return path
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

var openFailures = []struct {
	err            error
	check, summary string
	fix            string
}{
	{vault.ErrNotInitialized, "vault", "there is no vault in the data dir yet", fixInitVault},
	{vault.ErrCorrupt, "vault", "the vault database is corrupt or only partly initialised", "restore the vault from a backup, or move it aside and run `addition init`"},
	{vault.ErrSchemaTooNew, "vault", "the vault was written by a newer version of addition", "upgrade addition"},
	{vault.ErrInsecureHome, "vault", "the data dir is not private to this user", "make the data dir accessible to your user only (chmod 700)"},
	{vault.ErrNoMasterKey, "master-key", "the master key is not available from its recorded source", "restore the master key (OS keychain entry, key file or ADDITION_MASTER_KEY)"},
	{vault.ErrWrongMasterKey, "master-key", "the master key does not match this vault", "set ADDITION_MASTER_KEY to the key this vault was created with"},
	{crypto.ErrInvalidKey, "master-key", "ADDITION_MASTER_KEY is not a valid key", "fix or unset ADDITION_MASTER_KEY"},
	{vault.ErrKeychainUnavailable, "master-key", "the OS keychain is unavailable", "unlock the OS keychain, or set ADDITION_MASTER_KEY to the vault's key"},
	{vault.ErrInsecureKeyFile, "master-key", "the key file is not a private regular file", "make the key file a regular file readable only by you (chmod 600)"},
	{vault.ErrKeyFileUnsupported, "master-key", "key files are not supported on Windows", "set ADDITION_MASTER_KEY to the vault's key"},
}

// clientRoots are the identity roots whose OAuth client doctor checks: those that say how to set one up.
func clientRoots(cat *catalog.Catalog) []string {
	var roots []string
	for _, p := range cat.Providers() {
		if p.IdentityRoot == p.ID && p.Doctor.ClientHint != "" && len(cat.Apps(p.ID)) > 0 {
			roots = append(roots, p.ID)
		}
	}
	return roots
}

// clientChecks reports the OAuth client of each root that needs one. A fresh vault is nudged towards the
// roots that say how to set one up; otherwise the roots are those of the stored OAuth credentials, so a
// vault of token accounts is never asked for a client, and a root without a hint is still reported.
func (a *accountSession) clientChecks(clientIDs map[string]string, snap vault.Snapshot) []doctorCheck {
	var checks []doctorCheck
	for _, id := range a.rootsNeedingAClient(snap) {
		check := doctorCheck{ID: "oauth-client:" + id, Status: checkOK, Summary: "an OAuth client is stored for " + id + " apps"}
		if _, stored := clientIDs[id]; !stored {
			check = doctorCheck{
				ID: check.ID, Status: checkWarn, actionable: true, Summary: "no OAuth client is stored for " + id + " apps",
				Fix: "addition client add " + id + " --file client_secret.json", Hint: a.provider(id).Doctor.ClientHint,
			}
		}
		checks = append(checks, check)
	}
	return checks
}

func (a *accountSession) rootsNeedingAClient(snap vault.Snapshot) []string {
	if len(snap.Accounts) == 0 {
		return clientRoots(a.catalog)
	}
	var roots []string
	for _, acct := range snap.Accounts {
		cred, found := snap.Credentials[acct.ID]
		p, known := a.catalog.Provider(acct.Provider)
		if found && cred.Kind == vault.KindOAuth2 && known && !slices.Contains(roots, p.IdentityRoot) {
			roots = append(roots, p.IdentityRoot)
		}
	}
	slices.Sort(roots)
	return roots
}

// checkedAccounts are the account checks plus the accounts that passed them, which are safe to probe.
type checkedAccounts struct {
	checks  []doctorCheck
	all     []vault.Account
	healthy []vault.Account
}

func (a *accountSession) accountChecks(snap vault.Snapshot, now time.Time) checkedAccounts {
	out := checkedAccounts{all: snap.Accounts}
	for _, acct := range snap.Accounts {
		cred, found := snap.Credentials[acct.ID]
		check := a.accountCheck(acct, cred, found)
		out.checks = append(out.checks, check)
		if check.Code != checkOK {
			continue
		}
		out.healthy = append(out.healthy, acct)
		if age, aged := a.tokenAgeCheck(acct, cred, now); aged {
			out.checks = append(out.checks, age)
		}
	}
	return out
}

// accountCheck is the offline verdict of `accounts check` on one account, with the fix for what is wrong.
func (a *accountSession) accountCheck(acct vault.Account, cred vault.Credential, found bool) doctorCheck {
	kind := credentialKind(cred, found)
	check := doctorCheck{ID: "account:" + acct.Provider + ":" + acct.Alias, Status: checkFail, Fix: reconnectCommand(a.provider(acct.Provider), acct, kind)}
	status := a.statusOf(acct)
	if status == statusLegacy {
		check.Code, check.Summary = codeLegacy, legacyAccountError(a.catalog, acct).Message
		check.Fix = fmt.Sprintf("addition accounts remove %s:%s", acct.Provider, acct.Alias)
		return check
	}
	if status != vault.AccountActive {
		check.Code, check.Summary = codeNeedsReauth, "the provider refused this account's tokens; it must be reconnected"
		return check
	}
	switch _, missing := readiness(a.catalog, acct, kind); {
	case !found:
		check.Code, check.Summary = codeNoCredential, "no tokens are stored for this account"
	case cred.RefreshToken == "" && kind != vault.KindAPIKey:
		check.Code, check.Summary = codeNoRefreshToken, "no refresh token is stored, so the account stops working when its access token expires"
	case len(missing) > 0:
		check.Code, check.Summary = codeScopesMissing, fmt.Sprintf("the account lacks scopes the app needs: %v", missing)
	default:
		check.Status, check.Code, check.Summary, check.Fix = checkOK, checkOK, "tokens and scopes are in place", ""
		if kind == vault.KindAPIKey {
			check.Summary = "the token is in place"
		}
	}
	return check
}

// tokenAgeCheck warns when a connection is old enough that the provider may have expired its refresh token.
func (a *accountSession) tokenAgeCheck(acct vault.Account, cred vault.Credential, now time.Time) (doctorCheck, bool) {
	p, found := a.catalog.Provider(acct.Provider)
	if !found || p.Doctor.TokenAgeWarnDays == 0 || cred.Kind == vault.KindAPIKey {
		return doctorCheck{}, false
	}
	age := now.Sub(acct.UpdatedAt)
	if age < time.Duration(p.Doctor.TokenAgeWarnDays)*24*time.Hour {
		return doctorCheck{}, false
	}
	return doctorCheck{
		ID: "account:" + acct.Provider + ":" + acct.Alias + ":token-age", Status: checkWarn,
		Summary: fmt.Sprintf("this account was connected %d days ago", int(age/(24*time.Hour))), Hint: p.Doctor.TokenAgeHint,
	}, true
}

const activationBase = "https://console.developers.google.com/apis/api/"

var projectNumberPattern = regexp.MustCompile(`^[0-9]{6,20}$`)

// projectNumber is the Cloud project number a Google OAuth client id starts with, or "" for any other shape.
func projectNumber(clientID string) string {
	number, _, dashed := strings.Cut(clientID, "-")
	if dashed && projectNumberPattern.MatchString(number) {
		return number
	}
	return ""
}

// activationURL is where the user enables service; the project is left out when it is not known.
// Nothing a provider sent goes into it: only the catalog's service and the stored client id.
func activationURL(service, clientID string) string {
	link := activationBase + service + "/overview"
	if project := projectNumber(clientID); project != "" {
		link += "?project=" + project
	}
	return link
}

// cloudSetupChecks lists, once per connected app, the cloud API its account needs enabled.
// Token accounts have no Cloud project, so they get none.
func (a *accountSession) cloudSetupChecks(snap vault.Snapshot, clientIDs map[string]string) []doctorCheck {
	var apps []string
	for _, acct := range snap.Accounts {
		if snap.Credentials[acct.ID].Kind == vault.KindAPIKey {
			continue
		}
		if p, ok := a.catalog.Provider(acct.Provider); ok && a.catalog.Connectable(p.ID) && p.API.Service != "" && !slices.Contains(apps, p.ID) {
			apps = append(apps, p.ID)
		}
	}
	slices.Sort(apps)
	var checks []doctorCheck
	for _, id := range apps {
		p := a.provider(id)
		checks = append(checks, doctorCheck{
			ID: "cloud-setup:" + id, Status: checkWarn, Summary: p.API.Service + " must be enabled in the OAuth client's Cloud project",
			Hint: "Enable the " + p.API.Service + " API in the Cloud project that owns your OAuth client: " + activationURL(p.API.Service, clientIDs[p.IdentityRoot]),
		})
	}
	return checks
}

func (a *accountSession) provider(id string) *catalog.Provider {
	p, _ := a.catalog.Provider(id)
	return p
}
