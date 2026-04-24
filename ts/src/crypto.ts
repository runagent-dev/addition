import { createCipheriv, createDecipheriv, randomBytes } from 'node:crypto';
import { EncryptionError } from './errors.js';

const ALGORITHM = 'aes-256-gcm';
const IV_BYTES = 12;
const TAG_BYTES = 16;
const KEY_BYTES = 32;
const KEY_HEX_LEN = KEY_BYTES * 2;

function parseKey(hexKey: string): Buffer {
  if (typeof hexKey !== 'string' || hexKey.length !== KEY_HEX_LEN || !/^[0-9a-fA-F]+$/.test(hexKey)) {
    throw new EncryptionError(
      `encryption key must be ${KEY_HEX_LEN} hex characters (${KEY_BYTES} bytes)`,
    );
  }
  return Buffer.from(hexKey, 'hex');
}

/**
 * Encrypts UTF-8 plaintext with AES-256-GCM.
 * Output: base64(iv[12] || tag[16] || ciphertext).
 */
export function encrypt(plaintext: string, hexKey: string): string {
  const key = parseKey(hexKey);
  const iv = randomBytes(IV_BYTES);
  const cipher = createCipheriv(ALGORITHM, key, iv);
  const ciphertext = Buffer.concat([cipher.update(plaintext, 'utf8'), cipher.final()]);
  const tag = cipher.getAuthTag();
  return Buffer.concat([iv, tag, ciphertext]).toString('base64');
}

/**
 * Decrypts a ciphertext produced by `encrypt`. Throws `EncryptionError` on
 * bad key, malformed input, or auth-tag failure.
 */
export function decrypt(ciphertext: string, hexKey: string): string {
  const key = parseKey(hexKey);

  let raw: Buffer;
  try {
    raw = Buffer.from(ciphertext, 'base64');
  } catch (err) {
    throw new EncryptionError('ciphertext is not valid base64', err);
  }
  if (raw.length < IV_BYTES + TAG_BYTES) {
    throw new EncryptionError('ciphertext is too short to contain IV and auth tag');
  }

  const iv = raw.subarray(0, IV_BYTES);
  const tag = raw.subarray(IV_BYTES, IV_BYTES + TAG_BYTES);
  const data = raw.subarray(IV_BYTES + TAG_BYTES);

  try {
    const decipher = createDecipheriv(ALGORITHM, key, iv);
    decipher.setAuthTag(tag);
    const plaintext = Buffer.concat([decipher.update(data), decipher.final()]);
    return plaintext.toString('utf8');
  } catch (err) {
    throw new EncryptionError('decryption failed (wrong key or tampered ciphertext)', err);
  }
}
