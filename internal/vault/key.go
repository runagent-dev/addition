package vault

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

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

const (
	envMasterKey = "ADDN_MASTER_KEY"
	keyFileName  = "master.key"

	// KeychainService is the OS keychain service; the account is the vault's home path.
	KeychainService = "addition"
)

const canaryPlaintext = "addition master key canary"

// resolveKey follows invariant 2: env, then keychain, then the key file only if init opted into it.
func resolveKey(env Env, home string, recorded KeySource) (crypto.Key, error) {
	if hex := env.Getenv(envMasterKey); hex != "" {
		key, err := crypto.ParseKey(hex)
		if err != nil {
			return crypto.Key{}, fmt.Errorf("%s: %w", envMasterKey, err)
		}
		return key, nil
	}
	if hex, err := keyring.Get(KeychainService, home); err == nil {
		key, err := crypto.ParseKey(hex)
		if err != nil {
			return crypto.Key{}, fmt.Errorf("keychain entry for %s: %w", home, err)
		}
		return key, nil
	}
	if recorded == KeySourceFile {
		return readKeyFile(env, home)
	}
	return crypto.Key{}, noKeyError(recorded, home)
}

func noKeyError(recorded KeySource, home string) error {
	switch recorded {
	case KeySourceEnv:
		return fmt.Errorf("%w: this vault was initialised with %s; set it to the same key", ErrNoMasterKey, envMasterKey)
	case KeySourceFile:
		return fmt.Errorf("%w: key file %s is missing; restore it or set %s", ErrNoMasterKey, filepath.Join(home, keyFileName), envMasterKey)
	default:
		return fmt.Errorf("%w: the OS keychain has no key for %s or is unavailable; unlock the keychain or set %s", ErrNoMasterKey, home, envMasterKey)
	}
}

func readKeyFile(env Env, home string) (crypto.Key, error) {
	path := filepath.Join(home, keyFileName)
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return crypto.Key{}, noKeyError(KeySourceFile, home)
	}
	if err != nil {
		return crypto.Key{}, err
	}
	if env.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return crypto.Key{}, fmt.Errorf("%w: %s has mode %o; run chmod 600 %s", ErrInsecureKeyFile, path, info.Mode().Perm(), path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return crypto.Key{}, err
	}
	key, err := crypto.ParseKey(string(content))
	if err != nil {
		return crypto.Key{}, fmt.Errorf("%s: %w", path, err)
	}
	return key, nil
}

// provisionKey finds or creates the key for a new vault without overwriting any existing one.
func provisionKey(env Env, home string, useKeyFile bool) (crypto.Key, KeySource, error) {
	if hex := env.Getenv(envMasterKey); hex != "" {
		key, err := crypto.ParseKey(hex)
		if err != nil {
			return crypto.Key{}, "", fmt.Errorf("%s: %w", envMasterKey, err)
		}
		return key, KeySourceEnv, nil
	}
	if useKeyFile {
		key, err := provisionKeyFile(env, home)
		return key, KeySourceFile, err
	}
	key, err := provisionKeychain(home)
	return key, KeySourceKeychain, err
}

func provisionKeyFile(env Env, home string) (crypto.Key, error) {
	path := filepath.Join(home, keyFileName)
	if _, err := os.Stat(path); err == nil {
		return readKeyFile(env, home)
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		return crypto.Key{}, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return crypto.Key{}, fmt.Errorf("create key file: %w", err)
	}
	if _, err := f.WriteString(key.Hex()); err != nil {
		_ = f.Close()
		return crypto.Key{}, fmt.Errorf("write key file: %w", err)
	}
	if err := f.Close(); err != nil {
		return crypto.Key{}, fmt.Errorf("write key file: %w", err)
	}
	return key, nil
}

func provisionKeychain(home string) (crypto.Key, error) {
	hex, err := keyring.Get(KeychainService, home)
	if err == nil {
		key, err := crypto.ParseKey(hex)
		if err != nil {
			return crypto.Key{}, fmt.Errorf("keychain entry for %s: %w", home, err)
		}
		return key, nil
	}
	if !errors.Is(err, keyring.ErrNotFound) {
		return crypto.Key{}, keychainUnavailable(fmt.Sprintf(" (%v)", err))
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		return crypto.Key{}, err
	}
	// The Set error is dropped: it is the one call that carries the key.
	if err := keyring.Set(KeychainService, home, key.Hex()); err != nil {
		return crypto.Key{}, keychainUnavailable(" (storing the key failed)")
	}
	return key, nil
}

func keychainUnavailable(detail string) error {
	return fmt.Errorf("%w%s: run `addn init --key-file` to keep the key in a 0600 file, or set %s", ErrKeychainUnavailable, detail, envMasterKey)
}

func newCanary(key crypto.Key) (string, error) {
	return crypto.Encrypt(key, []byte(canaryPlaintext))
}

func checkCanary(key crypto.Key, canary string) error {
	plaintext, err := crypto.Decrypt(key, canary)
	if err != nil || string(plaintext) != canaryPlaintext {
		return fmt.Errorf("%w: it cannot decrypt this vault's canary", ErrWrongMasterKey)
	}
	return nil
}
