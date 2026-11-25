import { spawnSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";

import { ShimError } from "./errors.mjs";
import { assetFor } from "./platform.mjs";

const DEFAULT_BASE = "https://github.com/runagent-dev/addition/releases";
const MAX_CHECKSUMS_BYTES = 1024 * 1024;
const MAX_ARCHIVE_BYTES = 100 * 1024 * 1024;
const MAX_REDIRECTS = 5;
const LOOPBACK = new Set(["127.0.0.1", "localhost", "[::1]"]);

/** The release origin: https, or http on the loopback for tests and local mirrors. */
export function releaseBase(env = process.env) {
  const raw = (env.ADDITION_BASE_URL || DEFAULT_BASE).replace(/\/+$/, "");
  let url;
  try {
    url = new URL(raw);
  } catch {
    throw new ShimError("insecure-base-url", `ADDITION_BASE_URL is not a URL: ${raw}`);
  }
  const loopbackHttp = url.protocol === "http:" && LOOPBACK.has(url.hostname);
  if (url.protocol !== "https:" && !loopbackHttp) {
    throw new ShimError("insecure-base-url", "ADDITION_BASE_URL must be https, or http on 127.0.0.1 or localhost");
  }
  return { base: raw, allowHttp: loopbackHttp };
}

const isTransient = (status) => status === 408 || status === 429 || status >= 500;

/** GET with a few retries, following redirects only to https (or the loopback origin itself). */
async function get(url, origin, version) {
  let response;
  for (let attempt = 1; ; attempt++) {
    try {
      response = await fetchFollowing(url, origin);
      if (attempt === 4 || !isTransient(response.status)) break;
      await response.body?.cancel();
    } catch (error) {
      if (error instanceof ShimError || attempt === 4) throw error;
    }
    await new Promise((resolve) => setTimeout(resolve, 500 * 2 ** (attempt - 1)));
  }
  if (response.status === 404) {
    throw new ShimError(
      "release-not-found",
      `${url} does not exist. If ${version} was just released, try again in a few minutes.`,
    );
  }
  if (!response.ok) throw new ShimError("download-failed", `GET ${url} returned HTTP ${response.status}`);
  return response;
}

async function fetchFollowing(url, origin) {
  for (let hops = 0; hops <= MAX_REDIRECTS; hops++) {
    const response = await fetch(url, { redirect: "manual" });
    const location = response.headers.get("location");
    if (response.status < 300 || response.status >= 400 || !location) return response;
    await response.body?.cancel();
    url = new URL(location, url).href;
    const next = new URL(url);
    if (next.protocol !== "https:" && !(origin.allowHttp && next.protocol === "http:" && LOOPBACK.has(next.hostname))) {
      throw new ShimError("insecure-redirect", `the release server redirected to a non-https address: ${next.origin}`);
    }
  }
  throw new ShimError("download-failed", "too many redirects");
}

async function readText(url, origin, version, maxBytes) {
  const response = await get(url, origin, version);
  const body = Buffer.from(await response.arrayBuffer());
  if (body.length > maxBytes) throw new ShimError("download-failed", `${url} is larger than ${maxBytes} bytes`);
  return body.toString("utf8");
}

async function download(url, file, origin, version) {
  const response = await get(url, origin, version);
  const hash = crypto.createHash("sha256");
  const out = fs.openSync(file, "w");
  let received = 0;
  try {
    for await (const chunk of response.body) {
      received += chunk.byteLength;
      if (received > MAX_ARCHIVE_BYTES) {
        throw new ShimError("download-failed", `${url} is larger than ${MAX_ARCHIVE_BYTES} bytes`);
      }
      hash.update(chunk);
      fs.writeSync(out, chunk);
    }
  } finally {
    fs.closeSync(out);
  }
  return hash.digest("hex");
}

function expectedChecksum(sums, archive) {
  for (const line of sums.split(/\r?\n/)) {
    const match = /^([0-9a-f]{64})\s+\*?(.+)$/i.exec(line.trim());
    if (match && match[2] === archive) return match[1].toLowerCase();
  }
  throw new ShimError("asset-not-listed", `${archive} is not listed in the release's checksums.txt`);
}

function extract(archivePath, stagingDir, binary) {
  const tar = process.platform === "win32" ? `${process.env.SystemRoot}\\System32\\tar.exe` : "tar";
  const done = spawnSync(tar, ["-xf", archivePath, "-C", stagingDir, binary], { encoding: "utf8" });
  const extracted = path.join(stagingDir, binary);
  if (done.status !== 0 || !fs.existsSync(extracted)) {
    throw new ShimError("bad-archive", `the archive does not contain ${binary}`);
  }
  if (process.platform !== "win32") fs.chmodSync(extracted, 0o755);
  return extracted;
}

function confirmVersion(binaryPath, version) {
  const ran = spawnSync(binaryPath, ["version"], { encoding: "utf8", timeout: 30_000, killSignal: "SIGKILL" });
  if (ran.status !== 0) {
    const why = ran.error?.message || ran.stderr?.trim() || `exit ${ran.signal || ran.status}`;
    throw new ShimError("binary-failed", `the downloaded binary does not run on this machine: ${why}`);
  }
  let reported;
  try {
    reported = JSON.parse(ran.stdout).version;
  } catch {
    reported = undefined;
  }
  if (reported !== version) {
    throw new ShimError("version-mismatch", `the downloaded binary reports ${reported}, expected ${version}`);
  }
}

const marker = ".install-complete";

function isComplete(dir, version) {
  try {
    return fs.readFileSync(path.join(dir, marker), "utf8").trim() === version;
  } catch {
    return false;
  }
}

/** Rename staging into place; a complete install published meanwhile by another process wins. */
function publish(staging, dir, version) {
  try {
    fs.renameSync(staging, dir);
    return;
  } catch (error) {
    if (isComplete(dir, version)) return;
    if (!fs.existsSync(dir)) throw error;
  }
  const lock = `${dir}.lock`;
  try {
    fs.mkdirSync(lock);
  } catch (error) {
    if (error.code !== "EEXIST") throw error;
    throw new ShimError("install-busy", `another addition is installing ${version}. If none is running, delete ${lock} and try again.`);
  }
  const stale = `${staging}.stale`;
  try {
    if (isComplete(dir, version)) return;
    fs.renameSync(dir, stale);
    try {
      fs.renameSync(staging, dir);
    } catch (error) {
      if (!isComplete(dir, version)) throw error;
    }
  } finally {
    fs.rmSync(lock, { recursive: true, force: true });
    fs.rmSync(stale, { recursive: true, force: true });
  }
}

/** The path of the verified binary for version, downloaded and checked on first use. */
export async function ensureInstalled({ version, home, env = process.env, progress = () => {} }) {
  const { archive, binary } = assetFor(process.platform, process.arch);
  const dir = path.join(home, "runtime", "versions", version);
  if (isComplete(dir, version)) return path.join(dir, binary);

  const origin = releaseBase(env);
  const root = `${origin.base}/download/v${version}`;
  progress(`downloading addition ${version}`);
  const expected = expectedChecksum(await readText(`${root}/checksums.txt`, origin, version, MAX_CHECKSUMS_BYTES), archive);

  fs.mkdirSync(path.dirname(dir), { recursive: true });
  const staging = fs.mkdtempSync(`${dir}.staging-`);
  try {
    const file = path.join(staging, archive);
    if ((await download(`${root}/${archive}`, file, origin, version)) !== expected) {
      throw new ShimError("checksum-mismatch", `${archive} does not match the checksum in checksums.txt; nothing was installed`);
    }
    const extracted = extract(file, staging, binary);
    fs.rmSync(file);
    confirmVersion(extracted, version);
    fs.writeFileSync(path.join(staging, marker), `${version}\n`);
    publish(staging, dir, version);
  } finally {
    fs.rmSync(staging, { recursive: true, force: true });
  }
  return path.join(dir, binary);
}
