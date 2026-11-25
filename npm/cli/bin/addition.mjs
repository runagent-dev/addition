#!/usr/bin/env node
// Fetches the released addition binary for this platform on first use, verifies its checksum, then runs it.
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { envelope } from "../lib/errors.mjs";
import { dataHome } from "../lib/home.mjs";
import { ensureInstalled } from "../lib/install.mjs";

const { version } = JSON.parse(fs.readFileSync(path.join(import.meta.dirname, "../package.json"), "utf8"));

function fail(error) {
  process.stderr.write(`${envelope(error)}\n`);
  process.exit(1);
}

let binary;
try {
  binary = await ensureInstalled({
    version,
    home: dataHome(),
    progress: (line) => process.stderr.isTTY && process.stderr.write(`addition: ${line}\n`),
  });
} catch (error) {
  fail(error);
}

const child = spawn(binary, process.argv.slice(2), { stdio: "inherit" });
// A terminal's Ctrl-C already reaches the child, so SIGINT is not forwarded twice.
for (const signal of ["SIGTERM", "SIGHUP"]) {
  process.on(signal, () => child.kill(signal));
}
child.on("error", (error) => fail(error));
child.on("exit", (code, signal) => {
  process.exit(signal ? 128 + (os.constants.signals[signal] ?? 0) : (code ?? 1));
});
