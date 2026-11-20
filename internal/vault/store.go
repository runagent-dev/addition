package vault

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

const AccountActive = "active"

// Account is one connected identity, addressed by (Provider, Alias).
type Account struct {
	ID        int64
	Provider  string
	Alias     string
	Identity  string
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
}

// ops holds the store operations shared by Store and Tx.
type ops struct {
	q   querier
	key crypto.Key
	now func() time.Time
}

func (o ops) timestamp() string { return formatTime(o.now()) }

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

func (o ops) seal(plaintext string) (string, error) { return crypto.Encrypt(o.key, []byte(plaintext)) }

func (o ops) open(blob string) (string, error) {
	plaintext, err := crypto.Decrypt(o.key, blob)
	return string(plaintext), err
}

func (o ops) PutOAuthClient(ctx context.Context, c OAuthClient) error {
	secret, err := o.seal(c.ClientSecret)
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
	if c.ClientSecret, err = o.open(secret); err != nil {
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

// CreateAccount inserts a; an empty Status becomes AccountActive.
func (o ops) CreateAccount(ctx context.Context, a Account) (Account, error) {
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
		INSERT INTO accounts (provider, alias, identity, scopes, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.Provider, a.Alias, a.Identity, string(scopes), a.Status, formatTime(now), formatTime(now))
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

const accountColumns = `id, provider, alias, identity, scopes, status, created_at, updated_at`

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

// RemoveAccount deletes the account and, by cascade, its credential.
func (o ops) RemoveAccount(ctx context.Context, provider, alias string) error {
	res, err := o.q.ExecContext(ctx, `DELETE FROM accounts WHERE provider = ? AND alias = ?`, provider, alias)
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

func scanAccount(row scanner) (Account, error) {
	var a Account
	var scopes, created, updated string
	if err := row.Scan(&a.ID, &a.Provider, &a.Alias, &a.Identity, &scopes, &a.Status, &created, &updated); err != nil {
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
	access, err := o.seal(c.AccessToken)
	if err != nil {
		return err
	}
	var refresh, expires sql.NullString
	if c.RefreshToken != "" {
		blob, err := o.seal(c.RefreshToken)
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
			updated_at = excluded.updated_at`,
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
		SELECT access_token_encrypted, refresh_token_encrypted, token_type, expires_at, updated_at
		FROM credentials WHERE account_id = ?`, accountID).
		Scan(&access, &refresh, &c.TokenType, &expires, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, fmt.Errorf("credential for account %d: %w", accountID, ErrNotFound)
	}
	if err != nil {
		return Credential{}, err
	}
	if c.AccessToken, err = o.open(access); err != nil {
		return Credential{}, fmt.Errorf("credential for account %d: %w", accountID, err)
	}
	if refresh.Valid {
		if c.RefreshToken, err = o.open(refresh.String); err != nil {
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

// Audit records a sensitive event. details must never carry token or secret values.
func (o ops) Audit(ctx context.Context, event string, account *Account, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	body, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("audit details: %w", err)
	}
	var id sql.NullInt64
	var provider, alias sql.NullString
	if account != nil {
		id = sql.NullInt64{Int64: account.ID, Valid: account.ID != 0}
		provider = sql.NullString{String: account.Provider, Valid: true}
		alias = sql.NullString{String: account.Alias, Valid: true}
	}
	_, err = o.q.ExecContext(ctx, `
		INSERT INTO audit_log (event, account_id, provider, alias, details, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		event, id, provider, alias, string(body), o.timestamp())
	return err
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func isForeignKeyViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}
