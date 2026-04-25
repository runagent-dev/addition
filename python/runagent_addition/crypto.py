import base64
import os
import re

from cryptography.hazmat.primitives.ciphers.aead import AESGCM

from .errors import EncryptionError

_IV_BYTES = 12
_TAG_BYTES = 16
_KEY_BYTES = 32
_KEY_HEX_LEN = _KEY_BYTES * 2
_HEX_RE = re.compile(r"^[0-9a-fA-F]+$")


def _parse_key(hex_key: str) -> bytes:
    if (
        not isinstance(hex_key, str)
        or len(hex_key) != _KEY_HEX_LEN
        or not _HEX_RE.match(hex_key)
    ):
        raise EncryptionError(
            f"encryption key must be {_KEY_HEX_LEN} hex characters ({_KEY_BYTES} bytes)"
        )
    return bytes.fromhex(hex_key)


def encrypt(plaintext: str, hex_key: str) -> str:
    """Encrypt UTF-8 plaintext with AES-256-GCM.

    Output: base64(iv[12] || tag[16] || ciphertext)
    """
    key = _parse_key(hex_key)
    iv = os.urandom(_IV_BYTES)
    aesgcm = AESGCM(key)
    # cryptography returns ciphertext+tag concatenated
    ct_with_tag = aesgcm.encrypt(iv, plaintext.encode(), None)
    ciphertext = ct_with_tag[:-_TAG_BYTES]
    tag = ct_with_tag[-_TAG_BYTES:]
    wire = iv + tag + ciphertext
    return base64.b64encode(wire).decode()


def decrypt(ciphertext: str, hex_key: str) -> str:
    """Decrypt a ciphertext produced by `encrypt`. Raises EncryptionError on failure."""
    key = _parse_key(hex_key)

    try:
        raw = base64.b64decode(ciphertext)
    except Exception as exc:
        raise EncryptionError("ciphertext is not valid base64") from exc

    if len(raw) < _IV_BYTES + _TAG_BYTES:
        raise EncryptionError("ciphertext is too short to contain IV and auth tag")

    iv = raw[:_IV_BYTES]
    tag = raw[_IV_BYTES : _IV_BYTES + _TAG_BYTES]
    data = raw[_IV_BYTES + _TAG_BYTES :]

    try:
        aesgcm = AESGCM(key)
        # cryptography expects ciphertext+tag for decrypt
        plaintext_bytes = aesgcm.decrypt(iv, data + tag, None)
        return plaintext_bytes.decode()
    except Exception as exc:
        raise EncryptionError("decryption failed (wrong key or tampered ciphertext)") from exc
