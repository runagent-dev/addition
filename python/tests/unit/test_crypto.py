import base64
from pathlib import Path

import pytest
import yaml

from runagent_addition.crypto import decrypt, encrypt
from runagent_addition.errors import EncryptionError

KEY_A = "a" * 64
KEY_B = "b" * 64


class TestAES256GCM:
    def test_roundtrip_basic(self):
        plaintext = "hello, addition"
        ciphertext = encrypt(plaintext, KEY_A)
        assert decrypt(ciphertext, KEY_A) == plaintext

    def test_roundtrip_empty_string(self):
        ciphertext = encrypt("", KEY_A)
        assert decrypt(ciphertext, KEY_A) == ""

    def test_roundtrip_unicode_and_long(self):
        plaintext = "🔐 secret: " + "x" * 10_000
        ciphertext = encrypt(plaintext, KEY_A)
        assert decrypt(ciphertext, KEY_A) == plaintext

    def test_rejects_wrong_key(self):
        ciphertext = encrypt("secret", KEY_A)
        with pytest.raises(EncryptionError):
            decrypt(ciphertext, KEY_B)

    def test_rejects_tampered_ciphertext(self):
        ciphertext = encrypt("secret", KEY_A)
        raw = bytearray(base64.b64decode(ciphertext))
        raw[-1] ^= 0xFF
        tampered = base64.b64encode(bytes(raw)).decode()
        with pytest.raises(EncryptionError):
            decrypt(tampered, KEY_A)

    def test_output_format_iv12_tag16_ciphertext(self):
        plaintext = "hi"
        ciphertext = encrypt(plaintext, KEY_A)
        raw = base64.b64decode(ciphertext)
        # iv(12) + tag(16) + at least len(plaintext encoded) bytes
        assert len(raw) >= 12 + 16 + len(plaintext.encode())

    def test_fresh_iv_produces_unique_ciphertexts(self):
        a = encrypt("same", KEY_A)
        b = encrypt("same", KEY_A)
        assert a != b
        assert decrypt(a, KEY_A) == "same"
        assert decrypt(b, KEY_A) == "same"

    def test_rejects_invalid_key(self):
        with pytest.raises(EncryptionError):
            encrypt("x", "too-short")
        with pytest.raises(EncryptionError):
            encrypt("x", "z" * 64)  # not hex
        with pytest.raises(EncryptionError):
            encrypt("x", "a" * 63)  # wrong length
        with pytest.raises(EncryptionError):
            decrypt("AAAA", "too-short")

    def test_rejects_ciphertext_too_short(self):
        too_short = base64.b64encode(bytes(10)).decode()
        with pytest.raises(EncryptionError):
            decrypt(too_short, KEY_A)


class TestCrossLanguageInterop:
    """TS-encrypted vector must decrypt correctly in Python."""

    def test_decrypt_ts_vector(self):
        fixture_path = (
            Path(__file__).parents[3] / "shared" / "test-fixtures" / "crypto_interop.yaml"
        )
        fixture = yaml.safe_load(fixture_path.read_text())

        key = fixture["key"]
        vector = fixture["ts_vector"]
        assert decrypt(vector["ciphertext"], key) == vector["plaintext"]

    def test_encrypt_python_decrypts_with_same_key(self):
        """Python-encrypted blob must round-trip within Python (sanity check)."""
        fixture_path = (
            Path(__file__).parents[3] / "shared" / "test-fixtures" / "crypto_interop.yaml"
        )
        fixture = yaml.safe_load(fixture_path.read_text())

        key = fixture["key"]
        plaintext = fixture["ts_vector"]["plaintext"]
        py_ciphertext = encrypt(plaintext, key)
        assert decrypt(py_ciphertext, key) == plaintext
