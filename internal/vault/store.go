package vault

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/runagent-dev/addition/internal/crypto"
)

type OAuthClient struct {
	Provider     string
	ClientID     string
	ClientSecret string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const (
	AccountActive      = "active"
	AccountNeedsReauth = "needs-reauth"
)

// Account is one connected identity, addressed by (Provider, Alias). Subject is the provider's
// stable id for it; Identity is a display name, such as an email.
type Account struct {
	ID        int64
	Provider  string
	Alias     string
	Identity  string
	Subject   string
	Scopes    []string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Credential is an account's token set. Empty RefreshToken and zero ExpiresAt mean "none".
type Credential struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	ExpiresAt    time.Time
	UpdatedAt    time.Time
	// Revision counts the writes to this credential, so a reader can tell whether it changed whatever its token text.
	Revision int64
}

// ops holds the store operations shared by Store and Tx.
type ops struct {
	q       querier
	key     crypto.Key
	vaultID string
	now     func() time.Time
}

func (o ops) timestamp() string { return formatTime(o.now()) }

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

// slot names where a sealed value lives; its blob opens only in the same slot of the same vault.
type slot struct{ table, row, column string }

func clientSecretSlot(provider string) slot {
	return slot{"oauth_clients", provider, "client_secret_encrypted"}
}

func accessTokenSlot(accountID int64) slot {
	return slot{"credentials", strconv.FormatInt(accountID, 10), "access_token_encrypted"}
}

func refreshTokenSlot(accountID int64) slot {
	return slot{"credentials", strconv.FormatInt(accountID, 10), "refresh_token_encrypted"}
}

// Domain-separation string bound into every sealed value; changing it makes existing vaults unreadable.
const sealContextVersion = "addn-vault-aad/v1"

// sealContext is the AAD for a slot: length-prefixed parts, so no two slots share an encoding.
func sealContext(vaultID, table, row, column string) []byte {
	var aad []byte
	for _, part := range []string{sealContextVersion, vaultID, table, row, column} {
		aad = binary.AppendUvarint(aad, uint64(len(part)))
		aad = append(aad, part...)
	}
	return aad
}

func (o ops) seal(at slot, plaintext string) (string, error) {
	return crypto.EncryptWithAAD(o.key, []byte(plaintext), sealContext(o.vaultID, at.table, at.row, at.column))
}

func (o ops) open(at slot, blob string) (string, error) {
	plaintext, err := crypto.DecryptWithAAD(o.key, blob, sealContext(o.vaultID, at.table, at.row, at.column))
	return string(plaintext), err
}

func (o ops) PutOAuthClient(ctx context.Context, c OAuthClient) error {
	secret, err := o.seal(clientSecretSlot(c.Provider), c.ClientSecret)
	if err != nil {
		return err
	}
	now := o.timestamp()
	_, err = o.q.ExecContext(ctx, `
		INSERT INTO oauth_clients (provider, client_id, client_secret_encrypted, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (provider) DO UPDATE SET
			client_id = excluded.client_id,
			client_secret_encrypted = excluded.client_secret_encrypted,
			updated_at = excluded.updated_at`,
		c.Provider, c.ClientID, secret, now, now)
	return err
}

const oauthClientColumns = `provider, client_id, client_secret_encrypted, created_at, updated_at`

func (o ops) GetOAuthClient(ctx context.Context, provider string) (OAuthClient, error) {
	row := o.q.QueryRowContext(ctx, `SELECT `+oauthClientColumns+` FROM oauth_clients WHERE provider = ?`, provider)
	c, err := o.scanOAuthClient(row)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthClient{}, fmt.Errorf("oauth client for %s: %w", provider, ErrNotFound)
	}
	return c, err
}

func (o ops) ListOAuthClients(ctx context.Context) ([]OAuthClient, error) {
	rows, err := o.q.QueryContext(ctx, `SELECT `+oauthClientColumns+` FROM oauth_clients ORDER BY provider`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OAuthClient
	for rows.Next() {
		c, err := o.scanOAuthClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func (o ops) scanOAuthClient(row scanner) (OAuthClient, error) {
	var c OAuthClient
	var secret, created, updated string
	if err := row.Scan(&c.Provider, &c.ClientID, &secret, &created, &updated); err != nil {
		return OAuthClient{}, err
	}
	var err error
	if c.ClientSecret, err = o.open(clientSecretSlot(c.Provider), secret); err != nil {
		return OAuthClient{}, fmt.Errorf("oauth client for %s: %w", c.Provider, err)
	}
	if c.CreatedAt, err = parseTime(created); err != nil {
		return OAuthClient{}, err
	}
	if c.UpdatedAt, err = parseTime(updated); err != nil {
		return OAuthClient{}, err
	}
	return c, nil
}

// ErrInvalidAccount rejects a malformed provider id or alias; it never echoes the bad value.
var ErrInvalidAccount = errors.New("invalid account")

var (
	providerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	aliasPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

// ValidateAccountName checks a provider id and alias before anything is stored under them.
func ValidateAccountName(provider, alias string) error {
	if !providerIDPattern.MatchString(provider) {
		return fmt.Errorf("%w: provider must match %s", ErrInvalidAccount, providerIDPattern)
	}
	if !aliasPattern.MatchString(alias) {
		return fmt.Errorf("%w: alias must match %s", ErrInvalidAccount, aliasPattern)
	}
	return nil
}

// CreateAccount inserts a; an empty Status becomes AccountActive.
func (o ops) CreateAccount(ctx context.Context, a Account) (Account, error) {
	if err := ValidateAccountName(a.Provider, a.Alias); err != nil {
		return Account{}, err
	}
	if a.Status == "" {
		a.Status = AccountActive
	}
	if a.Scopes == nil {
		a.Scopes = []string{}
	}
	scopes, err := json.Marshal(a.Scopes)
	if err != nil {
		return Account{}, err
	}
	now := o.now().UTC()
	res, err := o.q.ExecContext(ctx, `
		INSERT INTO accounts (provider, alias, identity, subject, scopes, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Provider, a.Alias, a.Identity, a.Subject, string(scopes), a.Status, formatTime(now), formatTime(now))
	if isUniqueViolation(err) {
		return Account{}, fmt.Errorf("%s/%s: %w", a.Provider, a.Alias, ErrAliasTaken)
	}
	if err != nil {
		return Account{}, err
	}
	if a.ID, err = res.LastInsertId(); err != nil {
		return Account{}, err
	}
	a.CreatedAt, a.UpdatedAt = now, now
	return a, nil
}

const accountColumns = `id, provider, alias, identity, subject, scopes, status, created_at, updated_at`

func (o ops) GetAccount(ctx context.Context, provider, alias string) (Account, error) {
	row := o.q.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE provider = ? AND alias = ?`, provider, alias)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("account %s/%s: %w", provider, alias, ErrNotFound)
	}
	return a, err
}

// ListAccounts returns accounts ordered by provider then alias; an empty provider lists all.
func (o ops) ListAccounts(ctx context.Context, provider string) ([]Account, error) {
	rows, err := o.q.QueryContext(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE ? = '' OR provider = ? ORDER BY provider, alias`,
		provider, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateAccount stores a's identity, subject, scopes and status on the account named by its provider and alias.
func (o ops) UpdateAccount(ctx context.Context, a Account) (Account, error) {
	scopes, err := json.Marshal(nonNil(a.Scopes))
	if err != nil {
		return Account{}, err
	}
	now := o.now().UTC()
	res, err := o.q.ExecContext(ctx, `
		UPDATE accounts SET identity = ?, subject = ?, scopes = ?, status = ?, updated_at = ?
		WHERE provider = ? AND alias = ?`,
		a.Identity, a.Subject, string(scopes), a.Status, formatTime(now), a.Provider, a.Alias)
	if err := requireOneRow(res, err, a.Provider, a.Alias); err != nil {
		return Account{}, err
	}
	return o.GetAccount(ctx, a.Provider, a.Alias)
}

// RemoveAccount deletes the account, by cascade its credential, and its default marker.
func (o ops) RemoveAccount(ctx context.Context, provider, alias string) error {
	res, err := o.q.ExecContext(ctx, `DELETE FROM accounts WHERE provider = ? AND alias = ?`, provider, alias)
	if err := requireOneRow(res, err, provider, alias); err != nil {
		return err
	}
	_, err = o.q.ExecContext(ctx, `DELETE FROM meta WHERE key = ? AND value = ?`, defaultAccountKey(provider), alias)
	return err
}

func requireOneRow(res sql.Result, err error, provider, alias string) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("account %s/%s: %w", provider, alias, ErrNotFound)
	}
	return nil
}

func defaultAccountKey(provider string) string { return "default_account:" + provider }

// SetDefaultAccount makes alias the account used for provider when none is named. One
// statement checks the account exists and sets it, so a concurrent removal cannot leave it stale.
func (o ops) SetDefaultAccount(ctx context.Context, provider, alias string) error {
	res, err := o.q.ExecContext(ctx, `
		INSERT INTO meta (key, value)
		SELECT ?, alias FROM accounts WHERE provider = ? AND alias = ?
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		defaultAccountKey(provider), provider, alias)
	return requireOneRow(res, err, provider, alias)
}

// DefaultAccount returns provider's default alias, or ErrNotFound.
func (o ops) DefaultAccount(ctx context.Context, provider string) (string, error) {
	alias, err := getMeta(ctx, o.q, defaultAccountKey(provider))
	if errors.Is(err, ErrNotFound) {
		return "", fmt.Errorf("default account for %s: %w", provider, ErrNotFound)
	}
	return alias, err
}

func scanAccount(row scanner) (Account, error) {
	var a Account
	var scopes, created, updated string
	if err := row.Scan(&a.ID, &a.Provider, &a.Alias, &a.Identity, &a.Subject, &scopes, &a.Status, &created, &updated); err != nil {
		return Account{}, err
	}
	if err := json.Unmarshal([]byte(scopes), &a.Scopes); err != nil {
		return Account{}, fmt.Errorf("account %s/%s scopes: %w", a.Provider, a.Alias, err)
	}
	var err error
	if a.CreatedAt, err = parseTime(created); err != nil {
		return Account{}, err
	}
	if a.UpdatedAt, err = parseTime(updated); err != nil {
		return Account{}, err
	}
	return a, nil
}

// PutCredential replaces the account's credential.
func (o ops) PutCredential(ctx context.Context, accountID int64, c Credential) error {
	access, err := o.seal(accessTokenSlot(accountID), c.AccessToken)
	if err != nil {
		return err
	}
	var refresh, expires sql.NullString
	if c.RefreshToken != "" {
		blob, err := o.seal(refreshTokenSlot(accountID), c.RefreshToken)
		if err != nil {
			return err
		}
		refresh = sql.NullString{String: blob, Valid: true}
	}
	if !c.ExpiresAt.IsZero() {
		expires = sql.NullString{String: formatTime(c.ExpiresAt), Valid: true}
	}
	if c.TokenType == "" {
		c.TokenType = "Bearer"
	}
	_, err = o.q.ExecContext(ctx, `
		INSERT INTO credentials (account_id, access_token_encrypted, refresh_token_encrypted, token_type, expires_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (account_id) DO UPDATE SET
			access_token_encrypted = excluded.access_token_encrypted,
			refresh_token_encrypted = excluded.refresh_token_encrypted,
			token_type = excluded.token_type,
			expires_at = excluded.expires_at,
			updated_at = excluded.updated_at,
			revision = credentials.revision + 1`,
		accountID, access, refresh, c.TokenType, expires, o.timestamp())
	if isForeignKeyViolation(err) {
		return fmt.Errorf("account %d: %w", accountID, ErrNotFound)
	}
	return err
}

func (o ops) GetCredential(ctx context.Context, accountID int64) (Credential, error) {
	var c Credential
	var access, updated string
	var refresh, expires sql.NullString
	err := o.q.QueryRowContext(ctx, `
		SELECT access_token_encrypted, refresh_token_encrypted, token_type, expires_at, updated_at, revision
		FROM credentials WHERE account_id = ?`, accountID).
		Scan(&access, &refresh, &c.TokenType, &expires, &updated, &c.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, fmt.Errorf("credential for account %d: %w", accountID, ErrNotFound)
	}
	if err != nil {
		return Credential{}, err
	}
	if c.AccessToken, err = o.open(accessTokenSlot(accountID), access); err != nil {
		return Credential{}, fmt.Errorf("credential for account %d: %w", accountID, err)
	}
	if refresh.Valid {
		if c.RefreshToken, err = o.open(refreshTokenSlot(accountID), refresh.String); err != nil {
			return Credential{}, fmt.Errorf("credential for account %d: %w", accountID, err)
		}
	}
	if expires.Valid {
		if c.ExpiresAt, err = parseTime(expires.String); err != nil {
			return Credential{}, err
		}
	}
	if c.UpdatedAt, err = parseTime(updated); err != nil {
		return Credential{}, err
	}
	return c, nil
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func isForeignKeyViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}
