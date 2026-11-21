package cli

import (
	"errors"
	"fmt"
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
			"all is well). Without --live it reads the vault only and makes no network request.\n\n" +
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

// runDoctor reports a snapshot: everything it says about the vault comes from one read transaction,
// taken before any check is computed. With --live the probes run afterwards, against the live vault.
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
	store, err := vault.OpenWith(ctx, d.env, vault.OpenOptions{ReadOnly: true})
	if err != nil {
		failed, ok := openFailureChecks(cat, err)
		if !ok {
			return vaultError(err)
		}
		rep.Checks = append(rep.Checks, failed...)
		return finishDoctor(cmd, rep)
	}
	source := store.KeySource()
	snap, err := store.Snapshot(ctx)
	_ = store.Close()
	if err != nil {
		return vaultError(err)
	}

	session := &accountSession{catalog: cat}
	clientIDs := map[string]string{}
	for _, c := range snap.Clients {
		clientIDs[c.Provider] = c.ClientID
	}
	accounts := session.accountChecks(snap, d.now())
	rep.Checks = append(rep.Checks,
		doctorCheck{ID: "vault", Status: checkOK, Summary: fmt.Sprintf("the vault is initialised (schema %d)", vault.SchemaVersion)},
		doctorCheck{ID: "master-key", Status: checkOK, Summary: fmt.Sprintf("the master key unlocks the vault (recorded source: %s)", source)},
	)
	rep.Checks = append(rep.Checks, session.clientChecks(clientIDs)...)
	rep.Checks = append(rep.Checks, accounts.checks...)
	rep.Checks = append(rep.Checks, session.cloudSetupChecks(accounts.all, clientIDs)...)
	if live {
		probes, err := liveProbes(cmd, d, cat, accounts)
		if err != nil {
			return err
		}
		rep.Checks = append(rep.Checks, probes...)
	}
	return finishDoctor(cmd, rep)
}

// liveProbes reopens the vault for writing, since a probe may refresh a token and is audited, and
// leaves everything else in the data dir as it is.
func liveProbes(cmd *cobra.Command, d deps, cat *catalog.Catalog, accts checkedAccounts) ([]doctorCheck, error) {
	store, err := vault.OpenWith(cmd.Context(), d.env, vault.OpenOptions{KeepKeyTemps: true})
	if err != nil {
		return nil, vaultError(err)
	}
	defer store.Close()
	return (&accountSession{catalog: cat, store: store}).liveChecks(cmd, d, accts)
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
func openFailureChecks(cat *catalog.Catalog, err error) (checks []doctorCheck, ok bool) {
	for _, f := range openFailures {
		if !errors.Is(err, f.err) {
			continue
		}
		failed := doctorCheck{ID: f.check, Status: checkFail, Summary: f.summary, Fix: f.fix}
		if f.warn {
			failed.Status, failed.actionable = checkWarn, true
		}
		if f.check != "vault" {
			return []doctorCheck{{ID: "vault", Status: checkOK, Summary: "the vault database is there"}, failed}, true
		}
		checks = []doctorCheck{failed, {ID: "master-key", Status: checkSkip, Summary: "there is no usable vault to unlock"}}
		if errors.Is(err, vault.ErrInUse) {
			checks = append(checks, skippedDataChecks(cat)...)
		}
		return checks, true
	}
	return nil, false
}

// skippedDataChecks stand in for the client and account checks of a vault that was not read.
func skippedDataChecks(cat *catalog.Catalog) []doctorCheck {
	var checks []doctorCheck
	for _, id := range clientRoots(cat) {
		checks = append(checks, doctorCheck{ID: "oauth-client:" + id, Status: checkSkip, Summary: "not checked: the vault was not read"})
	}
	return append(checks, doctorCheck{ID: "accounts", Status: checkSkip, Summary: "not checked: the vault was not read"})
}

const fixInUse = "addition accounts list"

var openFailures = []struct {
	err            error
	check, summary string
	fix            string
	warn           bool
}{
	{vault.ErrNotInitialized, "vault", "there is no vault in the data dir yet", fixInitVault, false},
	{vault.ErrInUse, "vault", "the vault is in use or was not closed cleanly; run `addition accounts list` once, then run doctor again", fixInUse, true},
	{vault.ErrCorrupt, "vault", "the vault database is corrupt or only partly initialised", "restore the vault from a backup, or move it aside and run `addition init`", false},
	{vault.ErrSchemaTooNew, "vault", "the vault was written by a newer version of addition", "upgrade addition", false},
	{vault.ErrSchemaOlder, "vault", "the vault schema is older than this addition; any command that opens it for writing upgrades it", fixInUse, true},
	{vault.ErrInsecureHome, "vault", "the data dir is not private to this user", "make the data dir accessible to your user only (chmod 700)", false},
	{vault.ErrNoMasterKey, "master-key", "the master key is not available from its recorded source", "restore the master key (OS keychain entry, key file or ADDITION_MASTER_KEY)", false},
	{vault.ErrWrongMasterKey, "master-key", "the master key does not match this vault", "set ADDITION_MASTER_KEY to the key this vault was created with", false},
	{crypto.ErrInvalidKey, "master-key", "ADDITION_MASTER_KEY is not a valid key", "fix or unset ADDITION_MASTER_KEY", false},
	{vault.ErrKeychainUnavailable, "master-key", "the OS keychain is unavailable", "unlock the OS keychain, or set ADDITION_MASTER_KEY to the vault's key", false},
	{vault.ErrInsecureKeyFile, "master-key", "the key file is not a private regular file", "make the key file a regular file readable only by you (chmod 600)", false},
	{vault.ErrKeyFileUnsupported, "master-key", "key files are not supported on Windows", "set ADDITION_MASTER_KEY to the vault's key", false},
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

func (a *accountSession) clientChecks(clientIDs map[string]string) []doctorCheck {
	var checks []doctorCheck
	for _, id := range clientRoots(a.catalog) {
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
		if age, aged := a.tokenAgeCheck(acct, now); aged {
			out.checks = append(out.checks, age)
		}
	}
	return out
}

func reconnectFix(acct vault.Account) string {
	return fmt.Sprintf("addition connect %s --as %s", acct.Provider, acct.Alias)
}

// accountCheck is the offline verdict of `accounts check` on one account, with the fix for what is wrong.
func (a *accountSession) accountCheck(acct vault.Account, cred vault.Credential, found bool) doctorCheck {
	check := doctorCheck{ID: "account:" + acct.Provider + ":" + acct.Alias, Status: checkFail, Fix: reconnectFix(acct)}
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
	switch _, missing := readiness(a.catalog, acct); {
	case !found:
		check.Code, check.Summary = codeNoCredential, "no tokens are stored for this account"
	case cred.RefreshToken == "":
		check.Code, check.Summary = codeNoRefreshToken, "no refresh token is stored, so the account stops working when its access token expires"
	case len(missing) > 0:
		check.Code, check.Summary = codeScopesMissing, fmt.Sprintf("the account lacks scopes the app needs: %v", missing)
	default:
		check.Status, check.Code, check.Summary, check.Fix = checkOK, checkOK, "tokens and scopes are in place", ""
	}
	return check
}

// tokenAgeCheck warns when a connection is old enough that the provider may have expired its refresh token.
func (a *accountSession) tokenAgeCheck(acct vault.Account, now time.Time) (doctorCheck, bool) {
	p, found := a.catalog.Provider(acct.Provider)
	if !found || p.Doctor.TokenAgeWarnDays == 0 {
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
func (a *accountSession) cloudSetupChecks(accts []vault.Account, clientIDs map[string]string) []doctorCheck {
	var apps []string
	for _, acct := range accts {
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
