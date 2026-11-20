package crypto_test

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/runagent-dev/addition/internal/crypto"
)

var (
	keyA = mustParse(strings.Repeat("a", 64))
	keyB = mustParse(strings.Repeat("b", 64))
)

func mustParse(hex string) crypto.Key {
	k, err := crypto.ParseKey(hex)
	if err != nil {
		panic(err)
	}
	return k
}

func decode(t *testing.T, blob string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		t.Fatalf("blob is not standard base64: %v", err)
	}
	return raw
}

func TestRoundtrip(t *testing.T) {
	cases := map[string]string{
		"basic":        "hello, addition",
		"empty":        "",
		"unicode+long": "🔐 secret: " + strings.Repeat("x", 10_000),
	}
	for name, plaintext := range cases {
		t.Run(name, func(t *testing.T) {
			blob, err := crypto.Encrypt(keyA, []byte(plaintext))
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			got, err := crypto.Decrypt(keyA, blob)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if string(got) != plaintext {
				t.Errorf("roundtrip = %q, want %q", got, plaintext)
			}
		})
	}
}

func TestWireFormatIsIVThenTagThenCiphertext(t *testing.T) {
	plaintext := []byte("wire format")
	raw := decode(t, mustEncrypt(t, keyA, plaintext))

	if len(raw) != 12+16+len(plaintext) {
		t.Fatalf("len = %d, want %d", len(raw), 12+16+len(plaintext))
	}
	iv, tag, ciphertext := raw[:12], raw[12:28], raw[28:]

	block, _ := aes.NewCipher(keyA[:])
	gcm, _ := cipher.NewGCM(block)
	sealed := append(append([]byte{}, ciphertext...), tag...)
	got, err := gcm.Open(nil, iv, sealed, nil)
	if err != nil {
		t.Fatalf("stdlib GCM cannot open iv||tag||ciphertext: %v", err)
	}
	if string(got) != string(plaintext) {
		t.Errorf("plaintext = %q, want %q", got, plaintext)
	}
}

func TestFreshIVPerEncryption(t *testing.T) {
	a := mustEncrypt(t, keyA, []byte("same"))
	b := mustEncrypt(t, keyA, []byte("same"))
	if a == b {
		t.Fatal("two encryptions of the same plaintext produced identical blobs")
	}
	if string(decode(t, a)[:12]) == string(decode(t, b)[:12]) {
		t.Error("two encryptions reused the same IV")
	}
}

func TestDecryptRejects(t *testing.T) {
	good := mustEncrypt(t, keyA, []byte("secret"))
	tampered := decode(t, good)
	tampered[len(tampered)-1] ^= 0xff
	tamperedTag := decode(t, good)
	tamperedTag[12] ^= 0xff

	cases := map[string]struct {
		key  crypto.Key
		blob string
		want error
	}{
		"wrong key":            {keyB, good, crypto.ErrDecrypt},
		"tampered ciphertext":  {keyA, base64.StdEncoding.EncodeToString(tampered), crypto.ErrDecrypt},
		"tampered tag":         {keyA, base64.StdEncoding.EncodeToString(tamperedTag), crypto.ErrDecrypt},
		"too short for iv+tag": {keyA, base64.StdEncoding.EncodeToString(make([]byte, 27)), crypto.ErrMalformed},
		"not base64":           {keyA, "not base64!!", crypto.ErrMalformed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := crypto.Decrypt(tc.key, tc.blob)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseKeyRejectsAnythingButExactly64Hex(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"too short":        "too-short",
		"63 chars":         strings.Repeat("a", 63),
		"65 chars":         strings.Repeat("a", 65),
		"not hex":          strings.Repeat("z", 64),
		"trailing newline": strings.Repeat("a", 64) + "\n",
		"newline in 64":    strings.Repeat("a", 63) + "\n",
		"trailing CRLF":    strings.Repeat("a", 64) + "\r\n",
		"leading space":    " " + strings.Repeat("a", 64),
		"trailing space":   strings.Repeat("a", 64) + " ",
		"0x prefix":        "0x" + strings.Repeat("a", 62),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := crypto.ParseKey(input)
			if !errors.Is(err, crypto.ErrInvalidKey) {
				t.Fatalf("err = %v, want ErrInvalidKey", err)
			}
			if input != "" && strings.Contains(err.Error(), strings.TrimSpace(input)) {
				t.Errorf("error message echoes the key: %q", err)
			}
		})
	}
}

func TestParseKeyAcceptsMixedCase(t *testing.T) {
	lower := mustParse(strings.Repeat("ab", 32))
	upper := mustParse(strings.Repeat("AB", 32))
	if lower != upper {
		t.Error("hex case changed the parsed key")
	}
}

func TestGenerateKey(t *testing.T) {
	a, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	b, _ := crypto.GenerateKey()
	if a == b {
		t.Fatal("two generated keys are identical")
	}
	if len(a.Hex()) != 64 {
		t.Errorf("Hex() length = %d, want 64", len(a.Hex()))
	}
	if parsed := mustParse(a.Hex()); parsed != a {
		t.Error("ParseKey(k.Hex()) != k")
	}
}

func TestKeyFormattingIsRedacted(t *testing.T) {
	k := mustParse(strings.Repeat("c", 64))
	for _, verb := range []string{"%v", "%s", "%+v", "%#v", "%x", "%q"} {
		out := fmt.Sprintf(verb, k)
		if strings.Contains(strings.ToLower(out), "cccccccc") {
			t.Errorf("fmt %s leaks the key: %q", verb, out)
		}
	}
}

type interopVector struct {
	Plaintext  string `yaml:"plaintext"`
	Ciphertext string `yaml:"ciphertext"`
}

type interopFixture struct {
	Key      string        `yaml:"key"`
	TSVector interopVector `yaml:"ts_vector"`
	PyVector interopVector `yaml:"py_vector"`
}

func TestCryptoInteropFixture(t *testing.T) {
	data, err := os.ReadFile("../../shared/test-fixtures/crypto_interop.yaml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx interopFixture
	if err := yaml.Unmarshal(data, &fx); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	key, err := crypto.ParseKey(fx.Key)
	if err != nil {
		t.Fatalf("fixture key: %v", err)
	}

	vectors := map[string]interopVector{"typescript": fx.TSVector, "python": fx.PyVector}
	for name, v := range vectors {
		t.Run(name, func(t *testing.T) {
			if v.Ciphertext == "" {
				t.Fatal("fixture vector is missing")
			}
			got, err := crypto.Decrypt(key, v.Ciphertext)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if string(got) != v.Plaintext {
				t.Errorf("plaintext = %q, want %q", got, v.Plaintext)
			}

			again, err := crypto.Decrypt(key, mustEncrypt(t, key, []byte(v.Plaintext)))
			if err != nil || string(again) != v.Plaintext {
				t.Errorf("Go roundtrip = %q, %v; want %q", again, err, v.Plaintext)
			}
		})
	}
}

func mustEncrypt(t *testing.T, key crypto.Key, plaintext []byte) string {
	t.Helper()
	blob, err := crypto.Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	return blob
}

func TestAADBindsTheBlobToItsContext(t *testing.T) {
	here, there := []byte("vault/accounts/1/access"), []byte("vault/accounts/2/access")
	blob, err := crypto.EncryptWithAAD(keyA, []byte("token"), here)
	if err != nil {
		t.Fatalf("EncryptWithAAD: %v", err)
	}

	got, err := crypto.DecryptWithAAD(keyA, blob, here)
	if err != nil || string(got) != "token" {
		t.Fatalf("DecryptWithAAD = %q, %v; want token", got, err)
	}
	for name, aad := range map[string][]byte{"other context": there, "no context": nil} {
		if _, err := crypto.DecryptWithAAD(keyA, blob, aad); !errors.Is(err, crypto.ErrDecrypt) {
			t.Errorf("%s: err = %v, want ErrDecrypt", name, err)
		}
	}
	if _, err := crypto.Decrypt(keyA, blob); !errors.Is(err, crypto.ErrDecrypt) {
		t.Errorf("nil-AAD Decrypt of an AAD blob: err = %v, want ErrDecrypt", err)
	}
	if raw := decode(t, blob); len(raw) != 12+16+len("token") {
		t.Errorf("AAD blob is %d bytes, want iv||tag||ciphertext", len(raw))
	}
}

func TestNilAADIsTheSDKFormat(t *testing.T) {
	blob := mustEncrypt(t, keyA, []byte("sdk"))
	got, err := crypto.DecryptWithAAD(keyA, blob, nil)
	if err != nil || string(got) != "sdk" {
		t.Errorf("DecryptWithAAD(nil) of an Encrypt blob = %q, %v", got, err)
	}
}
