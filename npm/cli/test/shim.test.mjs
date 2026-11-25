import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";

import { assetFor } from "../lib/platform.mjs";

const shim = path.join(import.meta.dirname, "..", "bin", "addition.mjs");
const { version } = JSON.parse(fs.readFileSync(path.join(import.meta.dirname, "..", "package.json"), "utf8"));
const posix = process.platform !== "win32";
const skip = posix ? false : "the fake release builds its archive with a shell script";

const fakeBinary = (reported = version) => `#!/bin/sh
case "$1" in
  version) echo '{"version":"${reported}"}'; exit 0 ;;
  echo-args) shift; echo "args:$*"; exit 0 ;;
  echo-stdin) cat; exit 0 ;;
  exit-with) exit "$2" ;;
  wait) echo ready; trap 'echo got-term; exit 0' TERM; while true; do sleep 0.1; done ;;
esac
echo "unknown:$*"
`;

function archiveOf(files) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "addition-archive-"));
  for (const [name, body] of Object.entries(files)) {
    fs.writeFileSync(path.join(dir, name), body, { mode: 0o755 });
  }
  const out = path.join(dir, "out.tar.gz");
  const made = spawnSync("tar", ["-czf", out, "-C", dir, ...Object.keys(files)]);
  assert.equal(made.status, 0, String(made.stderr));
  const data = fs.readFileSync(out);
  fs.rmSync(dir, { recursive: true, force: true });
  return data;
}

const sha256 = (data) => crypto.createHash("sha256").update(data).digest("hex");

/** A release server on 127.0.0.1 holding one platform's archive; `hits` counts requests by file name. */
async function release({ archive, checksums, status = {} } = {}) {
  const { archive: name } = assetFor(process.platform, process.arch);
  const body = archive ?? archiveOf({ addition: fakeBinary() });
  const sums = checksums ?? `${sha256(body)}  ${name}\n`;
  const hits = {};
  const server = http.createServer((req, res) => {
    const file = path.basename(req.url);
    hits[file] = (hits[file] ?? 0) + 1;
    if (!req.url.startsWith(`/download/v${version}/`) || status[file]) {
      res.writeHead(status[file] ?? 404).end("no");
    } else if (file === "checksums.txt") {
      res.end(sums);
    } else if (file === name) {
      res.end(body);
    } else {
      res.writeHead(404).end("no");
    }
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  return {
    hits,
    base: `http://127.0.0.1:${server.address().port}`,
    name,
    close: () => new Promise((resolve) => server.close(resolve)),
  };
}

function home() {
  return fs.mkdtempSync(path.join(os.tmpdir(), "addition-home-"));
}

function run(args, { base, addHome, input, env = {} }) {
  return spawnSync(process.execPath, [shim, ...args], {
    input,
    encoding: "utf8",
    env: { ...process.env, ADDITION_HOME: addHome, ADDITION_BASE_URL: base, ...env },
  });
}

const cached = (addHome) => path.join(addHome, "runtime", "versions", version);

function errorOf(result) {
  const line = result.stderr.trim().split("\n").at(-1);
  return JSON.parse(line).error;
}

test("runs the released binary with the caller's arguments and returns its output", { skip }, async () => {
  const rel = await release();
  const addHome = home();
  try {
    const result = run(["echo-args", "gmail", "--as", "work"], { base: rel.base, addHome });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout.trim(), "args:gmail --as work");
    assert.equal(fs.readFileSync(path.join(cached(addHome), ".install-complete"), "utf8").trim(), version);
  } finally {
    await rel.close();
  }
});

test("the binary's exit code is the shim's exit code", { skip }, async () => {
  const rel = await release();
  const addHome = home();
  try {
    assert.equal(run(["exit-with", "7"], { base: rel.base, addHome }).status, 7);
    assert.equal(run(["exit-with", "3"], { base: rel.base, addHome }).status, 3);
  } finally {
    await rel.close();
  }
});

test("standard input reaches the binary, so a token can be piped to connect", { skip }, async () => {
  const rel = await release();
  const addHome = home();
  try {
    const result = run(["echo-stdin"], { base: rel.base, addHome, input: "line from the caller\n" });
    assert.equal(result.stdout, "line from the caller\n");
  } finally {
    await rel.close();
  }
});

test("the second run uses the cached binary and downloads nothing", { skip }, async () => {
  const rel = await release();
  const addHome = home();
  try {
    run(["echo-args", "one"], { base: rel.base, addHome });
    run(["echo-args", "two"], { base: rel.base, addHome });
    assert.equal(rel.hits[rel.name], 1);
    assert.equal(rel.hits["checksums.txt"], 1);
  } finally {
    await rel.close();
  }
});

test("an archive that does not match checksums.txt is refused and never run or cached", { skip }, async () => {
  const rel = await release({ checksums: `${"0".repeat(64)}  ${assetFor(process.platform, process.arch).archive}\n` });
  const addHome = home();
  try {
    const result = run(["echo-args", "x"], { base: rel.base, addHome });
    assert.equal(result.status, 1);
    assert.equal(errorOf(result).code, "checksum-mismatch");
    assert.equal(result.stdout, "");
    assert.equal(fs.existsSync(cached(addHome)), false);
  } finally {
    await rel.close();
  }
});

test("a release that does not list this platform's archive is refused", { skip }, async () => {
  const rel = await release({ checksums: `${"a".repeat(64)}  addition_plan9_amd64.tar.gz\n` });
  const addHome = home();
  try {
    const result = run(["echo-args", "x"], { base: rel.base, addHome });
    assert.equal(result.status, 1);
    assert.equal(errorOf(result).code, "asset-not-listed");
    assert.equal(fs.existsSync(cached(addHome)), false);
  } finally {
    await rel.close();
  }
});

test("a missing release says which version and suggests waiting", { skip }, async () => {
  const rel = await release({ status: { "checksums.txt": 404 } });
  const addHome = home();
  try {
    const result = run(["echo-args", "x"], { base: rel.base, addHome });
    assert.equal(result.status, 1);
    const error = errorOf(result);
    assert.equal(error.code, "release-not-found");
    assert.ok(error.message.includes(version), error.message);
  } finally {
    await rel.close();
  }
});

test("a binary that reports another version is refused and not cached", { skip }, async () => {
  const rel = await release({ archive: archiveOf({ addition: fakeBinary("9.9.9") }) });
  const addHome = home();
  try {
    const result = run(["echo-args", "x"], { base: rel.base, addHome });
    assert.equal(result.status, 1);
    assert.equal(errorOf(result).code, "version-mismatch");
    assert.equal(fs.existsSync(cached(addHome)), false);
  } finally {
    await rel.close();
  }
});

test("an archive without the binary is refused", { skip }, async () => {
  const rel = await release({ archive: archiveOf({ NOTICE: "not the binary" }) });
  const addHome = home();
  try {
    const result = run(["echo-args", "x"], { base: rel.base, addHome });
    assert.equal(result.status, 1);
    assert.equal(errorOf(result).code, "bad-archive");
    assert.equal(fs.existsSync(cached(addHome)), false);
  } finally {
    await rel.close();
  }
});

test("only https, or http on the loopback, is accepted as the release origin", { skip }, async () => {
  const addHome = home();
  for (const base of ["http://example.com/releases", "ftp://example.com/releases", "not a url"]) {
    const result = run(["echo-args", "x"], { base, addHome });
    assert.equal(result.status, 1, base);
    assert.equal(errorOf(result).code, "insecure-base-url", base);
  }
  assert.equal(fs.existsSync(path.join(addHome, "runtime")), false);
});

test("two runs at once both succeed and leave one complete install", { skip }, async () => {
  const rel = await release();
  const addHome = home();
  try {
    const runOne = () =>
      new Promise((resolve) => {
        const child = spawn(process.execPath, [shim, "echo-args", "ok"], {
          env: { ...process.env, ADDITION_HOME: addHome, ADDITION_BASE_URL: rel.base },
        });
        let out = "";
        child.stdout.on("data", (chunk) => (out += chunk));
        child.on("close", (code) => resolve({ code, out }));
      });
    const results = await Promise.all([runOne(), runOne()]);
    for (const result of results) {
      assert.deepEqual(result, { code: 0, out: "args:ok\n" });
    }
    assert.deepEqual(fs.readdirSync(path.join(addHome, "runtime", "versions")), [version]);
  } finally {
    await rel.close();
  }
});

test("SIGTERM to the shim reaches the binary", { skip }, async () => {
  const rel = await release();
  const addHome = home();
  try {
    const child = spawn(process.execPath, [shim, "wait"], {
      env: { ...process.env, ADDITION_HOME: addHome, ADDITION_BASE_URL: rel.base },
    });
    let out = "";
    child.stdout.on("data", (chunk) => (out += chunk));
    await new Promise((resolve) => child.stdout.once("data", resolve));
    child.kill("SIGTERM");
    const code = await new Promise((resolve) => child.on("close", resolve));
    assert.match(out, /got-term/);
    assert.equal(code, 0);
  } finally {
    await rel.close();
  }
});
