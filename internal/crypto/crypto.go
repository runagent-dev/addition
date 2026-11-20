// Package crypto seals vault secrets with AES-256-GCM.
//
// Wire format: base64(iv[12] || tag[16] || ciphertext), pinned by
// shared/test-fixtures/crypto_interop.yaml.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

const (
	keyBytes = 32
	ivBytes  = 12
	tagBytes = 16
)

var (
	ErrInvalidKey = errors.New("master key must be exactly 64 hex characters (32 bytes)")
	ErrMalformed  = errors.New("ciphertext is malformed")
	ErrDecrypt    = errors.New("decryption failed (wrong key or tampered ciphertext)")
)

// Key is an AES-256 key. It formats as "[redacted]" so it cannot leak through fmt.
type Key [keyBytes]byte

func (Key) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte("[redacted]")) }

// Hex returns the key's canonical 64-character form, for storing it in a key source.
func (k Key) Hex() string { return hex.EncodeToString(k[:]) }

func GenerateKey() (Key, error) {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		return Key{}, fmt.Errorf("generate key: %w", err)
	}
	return k, nil
}

// ParseKey accepts exactly 64 hex characters; surrounding whitespace is an error.
func ParseKey(s string) (Key, error) {
	var k Key
	if len(s) != 2*keyBytes {
		return Key{}, ErrInvalidKey
	}
	if _, err := hex.Decode(k[:], []byte(s)); err != nil {
		return Key{}, ErrInvalidKey
	}
	return k, nil
}

// Encrypt seals plaintext without associated data, as the SDK did.
func Encrypt(key Key, plaintext []byte) (string, error) { return EncryptWithAAD(key, plaintext, nil) }

// Decrypt opens a blob sealed without associated data.
func Decrypt(key Key, blob string) ([]byte, error) { return DecryptWithAAD(key, blob, nil) }

// EncryptWithAAD seals plaintext so that it opens only with the same aad; the wire format is unchanged.
func EncryptWithAAD(key Key, plaintext, aad []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	iv := make([]byte, ivBytes)
	if _, err := rand.Read(iv); err != nil {
		return "", fmt.Errorf("generate iv: %w", err)
	}
	sealed := gcm.Seal(nil, iv, plaintext, aad)
	ciphertext, tag := sealed[:len(sealed)-tagBytes], sealed[len(sealed)-tagBytes:]

	wire := make([]byte, 0, ivBytes+len(sealed))
	wire = append(wire, iv...)
	wire = append(wire, tag...)
	wire = append(wire, ciphertext...)
	return base64.StdEncoding.EncodeToString(wire), nil
}

func DecryptWithAAD(key Key, blob string, aad []byte) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		return nil, fmt.Errorf("%w: not base64", ErrMalformed)
	}
	if len(raw) < ivBytes+tagBytes {
		return nil, fmt.Errorf("%w: too short to hold iv and tag", ErrMalformed)
	}
	iv, tag, ciphertext := raw[:ivBytes], raw[ivBytes:ivBytes+tagBytes], raw[ivBytes+tagBytes:]

	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	sealed := make([]byte, 0, len(ciphertext)+tagBytes)
	sealed = append(sealed, ciphertext...)
	sealed = append(sealed, tag...)
	plaintext, err := gcm.Open(nil, iv, sealed, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

func newGCM(key Key) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
