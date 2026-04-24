import { describe, expect, it } from 'vitest';
import { encrypt, decrypt } from '../../src/crypto.js';
import { EncryptionError } from '../../src/errors.js';

// 64 hex chars = 32 bytes
const KEY_A = 'a'.repeat(64);
const KEY_B = 'b'.repeat(64);

describe('crypto — AES-256-GCM', () => {
  it('roundtrip: encrypt then decrypt returns the original plaintext', () => {
    const plaintext = 'hello, addition';
    const ciphertext = encrypt(plaintext, KEY_A);
    expect(decrypt(ciphertext, KEY_A)).toBe(plaintext);
  });

  it('roundtrip: handles empty string', () => {
    const ciphertext = encrypt('', KEY_A);
    expect(decrypt(ciphertext, KEY_A)).toBe('');
  });

  it('roundtrip: handles unicode and long plaintext', () => {
    const plaintext = '🔐 secret: ' + 'x'.repeat(10_000);
    const ciphertext = encrypt(plaintext, KEY_A);
    expect(decrypt(ciphertext, KEY_A)).toBe(plaintext);
  });

  it('rejects decryption with a different key (throws EncryptionError)', () => {
    const ciphertext = encrypt('secret', KEY_A);
    expect(() => decrypt(ciphertext, KEY_B)).toThrow(EncryptionError);
  });

  it('rejects tampered ciphertext (auth tag mismatch → throws)', () => {
    const ciphertext = encrypt('secret', KEY_A);
    // Flip a byte deep in the payload (past iv+tag=28 bytes when base64-decoded).
    const raw = Buffer.from(ciphertext, 'base64');
    raw[raw.length - 1] ^= 0xff;
    const tampered = raw.toString('base64');
    expect(() => decrypt(tampered, KEY_A)).toThrow(EncryptionError);
  });

  it('output format is base64(iv[12] || tag[16] || ciphertext)', () => {
    const plaintext = 'hi';
    const ciphertext = encrypt(plaintext, KEY_A);
    const raw = Buffer.from(ciphertext, 'base64');
    // iv(12) + tag(16) + ciphertext(>=plaintext bytes for GCM) = at least 28 + 2
    expect(raw.length).toBeGreaterThanOrEqual(12 + 16 + Buffer.byteLength(plaintext));
    // Decoding must not throw and must produce Buffer
    expect(Buffer.isBuffer(raw)).toBe(true);
  });

  it('two encryptions of the same plaintext produce different ciphertexts (fresh IV)', () => {
    const a = encrypt('same', KEY_A);
    const b = encrypt('same', KEY_A);
    expect(a).not.toBe(b);
    // But both decrypt to the same plaintext
    expect(decrypt(a, KEY_A)).toBe('same');
    expect(decrypt(b, KEY_A)).toBe('same');
  });

  it('rejects a key that is not 64 hex chars (throws EncryptionError)', () => {
    expect(() => encrypt('x', 'too-short')).toThrow(EncryptionError);
    expect(() => encrypt('x', 'z'.repeat(64))).toThrow(EncryptionError); // not hex
    expect(() => encrypt('x', 'a'.repeat(63))).toThrow(EncryptionError); // wrong length
    expect(() => decrypt('AAAA', 'too-short')).toThrow(EncryptionError);
  });

  it('rejects malformed ciphertext (too short to contain iv+tag)', () => {
    // Only 10 bytes base64-encoded — cannot contain a 12-byte IV.
    const tooShort = Buffer.alloc(10, 0).toString('base64');
    expect(() => decrypt(tooShort, KEY_A)).toThrow(EncryptionError);
  });
});
