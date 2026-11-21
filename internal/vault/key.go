package vault

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/zalando/go-keyring"

	"github.com/runagent-dev/addition/internal/crypto"
)

// KeySource records where a vault's master key lives.
type KeySource string

const (
	KeySourceEnv      KeySource = "env"
	KeySourceKeychain KeySource = "keychain"
	KeySourceFile     KeySource = "file"
)

func (s KeySource) valid() bool {
	return s == KeySourceEnv || s == KeySourceKeychain || s == KeySourceFile
}

const (
	envMasterKey = "ADDITION_MASTER_KEY"
	keyFileName  = "master.key"

	// KeychainService is the OS keychain service; the account is the vault's random ID.
	KeychainService = "addition"

	keyFileMaxBytes = 4096
)

const canaryPlaintext = "addition master key canary"

// resolveKey uses a set ADDITION_MASTER_KEY, else only the source the vault recorded at init.
func resolveKey(env Env, home *homeDir, rec initRecords) (crypto.Key, error) {
	if hex, ok := env.LookupEnv(envMasterKey); ok {
		return parseEnvKey(hex)
	}
	switch rec.source {
	case KeySourceKeychain:
		return keychainKey(rec.vaultID)
	case KeySourceFile:
		if env.GOOS == "windows" {
			return crypto.Key{}, ErrKeyFileUnsupported
		}
		key, err := readKeyFile(home)
		if errors.Is(err, fs.ErrNotExist) {
			return crypto.Key{}, fmt.Errorf("%w: key file %s is missing; restore it or set %s", ErrNoMasterKey, home.join(keyFileName), envMasterKey)
		}
		return key, err
	default:
		return crypto.Key{}, fmt.Errorf("%w: this vault was initialised with a key from the environment; set %s to that key", ErrNoMasterKey, envMasterKey)
	}
}

func parseEnvKey(hex string) (crypto.Key, error) {
	if hex == "" {
		return crypto.Key{}, fmt.Errorf("%s is set but empty; unset it or set it to the vault's key: %w", envMasterKey, crypto.ErrInvalidKey)
	}
	key, err := crypto.ParseKey(hex)
	if err != nil {
		return crypto.Key{}, fmt.Errorf("%s: %w", envMasterKey, err)
	}
	return key, nil
}

func keychainKey(vaultID string) (crypto.Key, error) {
	hex, err := keyring.Get(KeychainService, vaultID)
	if err != nil {
		return crypto.Key{}, fmt.Errorf("%w: the OS keychain has no key for vault %s or is unavailable; unlock the keychain or set %s", ErrNoMasterKey, vaultID, envMasterKey)
	}
	key, err := crypto.ParseKey(hex)
	if err != nil {
		return crypto.Key{}, fmt.Errorf("keychain entry for vault %s: %w", vaultID, err)
	}
	return key, nil
}

// readKeyFile reads the key through one handle that it has checked is a private regular file.
func readKeyFile(home *homeDir) (crypto.Key, error) {
	f, err := home.openKeyFile()
	if err != nil {
		return crypto.Key{}, err
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, keyFileMaxBytes))
	if err != nil {
		return crypto.Key{}, fmt.Errorf("read key file: %w", err)
	}
	key, err := crypto.ParseKey(string(content))
	if err != nil {
		return crypto.Key{}, fmt.Errorf("key file %s is empty, truncated or not a key; restore it or set %s: %w", f.Name(), envMasterKey, err)
	}
	return key, nil
}

// provisionKey finds or creates the key for a new vault; a set ADDITION_MASTER_KEY wins over opts.
func provisionKey(env Env, home *homeDir, vaultID string, useKeyFile bool) (crypto.Key, KeySource, error) {
	if hex, ok := env.LookupEnv(envMasterKey); ok {
		key, err := parseEnvKey(hex)
		return key, KeySourceEnv, err
	}
	if useKeyFile {
		key, err := provisionKeyFile(home)
		return key, KeySourceFile, err
	}
	key, err := provisionKeychain(vaultID)
	return key, KeySourceKeychain, err
}

// provisionKeyFile reuses a key file an interrupted init left behind, else publishes a new one.
func provisionKeyFile(home *homeDir) (crypto.Key, error) {
	key, err := readKeyFile(home)
	if !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}
	if key, err = crypto.GenerateKey(); err != nil {
		return crypto.Key{}, err
	}
	if err := home.writeKeyFile(key.Hex()); err != nil {
		return crypto.Key{}, err
	}
	return key, nil
}

func provisionKeychain(vaultID string) (crypto.Key, error) {
	if _, err := keyring.Get(KeychainService, vaultID); !errors.Is(err, keyring.ErrNotFound) {
		return crypto.Key{}, keychainUnavailable(fmt.Sprintf(" (%v)", err))
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		return crypto.Key{}, err
	}
	// The Set error is dropped: it is the one call that carries the key.
	if err := keyring.Set(KeychainService, vaultID, key.Hex()); err != nil {
		return crypto.Key{}, keychainUnavailable(" (storing the key failed)")
	}
	return key, nil
}

func keychainUnavailable(detail string) error {
	return fmt.Errorf("%w%s: run `addition init --key-file` to keep the key in a 0600 file, or set %s", ErrKeychainUnavailable, detail, envMasterKey)
}

func newVaultID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate vault id: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}

func newCanary(key crypto.Key, vaultID string) (string, error) {
	return crypto.EncryptWithAAD(key, []byte(canaryPlaintext), sealContext(vaultID, "meta", metaKeyCanary, "value"))
}

func checkCanary(key crypto.Key, rec initRecords) error {
	plaintext, err := crypto.DecryptWithAAD(key, rec.canary, sealContext(rec.vaultID, "meta", metaKeyCanary, "value"))
	if err != nil || string(plaintext) != canaryPlaintext {
		return fmt.Errorf("%w: it cannot decrypt this vault's canary", ErrWrongMasterKey)
	}
	return nil
}
