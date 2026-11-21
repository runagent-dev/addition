package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
			"and the probe are recorded like any other call.\n\n" +
			"Exit codes: 0 when ok; 3 when something needs you to act (the report is still on stdout);\n" +
			"1 when addition itself failed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runDoctor(cmd, d, live) },
	}
	cmd.Flags().BoolVar(&live, "live", false, "also probe each connected app's API once")
	return cmd
}

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
	store, err := vault.Open(ctx, d.env)
	if err != nil {
		failed, ok := openFailureChecks(err)
		if !ok {
			return vaultError(err)
		}
		rep.Checks = append(rep.Checks, failed...)
		return finishDoctor(cmd, rep)
	}
	defer store.Close()

	session := &accountSession{catalog: cat, store: store}
	rep.Checks = append(rep.Checks,
		doctorCheck{ID: "vault", Status: checkOK, Summary: fmt.Sprintf("the vault is initialised (schema %d)", vault.SchemaVersion)},
		doctorCheck{ID: "master-key", Status: checkOK, Summary: fmt.Sprintf("the master key unlocks the vault (recorded source: %s)", store.KeySource())},
	)
	clients, err := session.clientChecks(ctx)
	if err != nil {
		return err
	}
	accounts, err := session.accountChecks(ctx, d.now())
	if err != nil {
		return err
	}
	rep.Checks = append(rep.Checks, clients...)
	rep.Checks = append(rep.Checks, accounts.checks...)
	rep.Checks = append(rep.Checks, session.cloudSetupChecks(accounts.all)...)
	if live {
		probes, err := session.liveChecks(cmd, d, accounts)
		if err != nil {
			return err
		}
		rep.Checks = append(rep.Checks, probes...)
	}
	return finishDoctor(cmd, rep)
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
func openFailureChecks(err error) (checks []doctorCheck, ok bool) {
	for _, f := range openFailures {
		if !errors.Is(err, f.err) {
			continue
		}
		failed := doctorCheck{ID: f.check, Status: checkFail, Summary: f.summary, Fix: f.fix}
		if f.check == "vault" {
			return []doctorCheck{failed, {ID: "master-key", Status: checkSkip, Summary: "there is no usable vault to unlock"}}, true
		}
		return []doctorCheck{{ID: "vault", Status: checkOK, Summary: "the vault database is there"}, failed}, true
	}
	return nil, false
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

func (a *accountSession) clientChecks(ctx context.Context) ([]doctorCheck, error) {
	stored, err := a.store.ListOAuthClients(ctx)
	if err != nil {
		return nil, vaultError(err)
	}
	var checks []doctorCheck
	for _, p := range a.catalog.Providers() {
		if p.IdentityRoot != p.ID || p.Doctor.ClientHint == "" || len(a.catalog.Apps(p.ID)) == 0 {
			continue
		}
		check := doctorCheck{ID: "oauth-client:" + p.ID, Status: checkOK, Summary: "an OAuth client is stored for " + p.ID + " apps"}
		if !slices.ContainsFunc(stored, func(c vault.OAuthClient) bool { return c.Provider == p.ID }) {
			check = doctorCheck{
				ID: check.ID, Status: checkWarn, actionable: true, Summary: "no OAuth client is stored for " + p.ID + " apps",
				Fix: "addition client add " + p.ID + " --file client_secret.json", Hint: p.Doctor.ClientHint,
			}
		}
		checks = append(checks, check)
	}
	return checks, nil
}

// checkedAccounts are the account checks plus the accounts that passed them, which are safe to probe.
type checkedAccounts struct {
	checks  []doctorCheck
	all     []vault.Account
	healthy []vault.Account
}

func (a *accountSession) accountChecks(ctx context.Context, now time.Time) (checkedAccounts, error) {
	var out checkedAccounts
	var err error
	if out.all, err = a.store.ListAccounts(ctx, ""); err != nil {
		return out, vaultError(err)
	}
	for _, acct := range out.all {
		check, err := a.accountCheck(ctx, acct)
		if err != nil {
			return out, err
		}
		out.checks = append(out.checks, check)
		if check.Code != checkOK {
			continue
		}
		out.healthy = append(out.healthy, acct)
		if age, aged := a.tokenAgeCheck(acct, now); aged {
			out.checks = append(out.checks, age)
		}
	}
	return out, nil
}

func reconnectFix(acct vault.Account) string {
	return fmt.Sprintf("addition connect %s --as %s", acct.Provider, acct.Alias)
}

// accountCheck is the offline verdict of `accounts check` on one account, with the fix for what is wrong.
func (a *accountSession) accountCheck(ctx context.Context, acct vault.Account) (doctorCheck, error) {
	check := doctorCheck{ID: "account:" + acct.Provider + ":" + acct.Alias, Status: checkFail, Fix: reconnectFix(acct)}
	status := a.statusOf(acct)
	if status == statusLegacy {
		check.Code, check.Summary = codeLegacy, legacyAccountError(a.catalog, acct).Message
		check.Fix = fmt.Sprintf("addition accounts remove %s:%s", acct.Provider, acct.Alias)
		return check, nil
	}
	if status != vault.AccountActive {
		check.Code, check.Summary = codeNeedsReauth, "the provider refused this account's tokens; it must be reconnected"
		return check, nil
	}
	cred, found, err := a.credential(ctx, acct)
	if err != nil {
		return check, err
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
	return check, nil
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

// activationURL is where the user enables service; the project is left out when it is not known.
func activationURL(service, project string) string {
	link := activationBase + service + "/overview"
	if project != "" {
		link += "?project=" + project
	}
	return link
}

// cloudSetupChecks lists, once per connected app, the cloud API its account needs enabled.
func (a *accountSession) cloudSetupChecks(accts []vault.Account) []doctorCheck {
	var apps []string
	for _, acct := range accts {
		if p, ok := a.catalog.Provider(acct.Provider); ok && a.catalog.Connectable(p.ID) && p.API.Service != "" && !slices.Contains(apps, p.ID) {
			apps = append(apps, p.ID)
		}
	}
	slices.Sort(apps)
	var checks []doctorCheck
	for _, id := range apps {
		service := a.provider(id).API.Service
		checks = append(checks, doctorCheck{
			ID: "cloud-setup:" + id, Status: checkWarn, Summary: service + " must be enabled in the OAuth client's Cloud project",
			Hint: "Enable the " + service + " API in the Cloud project that owns your OAuth client: " + activationURL(service, ""),
		})
	}
	return checks
}

func (a *accountSession) provider(id string) *catalog.Provider {
	p, _ := a.catalog.Provider(id)
	return p
}
