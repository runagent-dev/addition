import os from "node:os";
import path from "node:path";

import { ShimError } from "./errors.mjs";

/** The data dir the binary uses: $ADDITION_HOME, else the platform's user data dir. */
export function dataHome(env = process.env, platform = process.platform, homedir = os.homedir()) {
  const explicit = env.ADDITION_HOME;
  if (explicit) {
    if (!path.isAbsolute(explicit)) {
      throw new ShimError("bad-home", "ADDITION_HOME must be an absolute path");
    }
    return explicit;
  }
  if (platform === "win32") {
    if (!env.LOCALAPPDATA) {
      throw new ShimError("bad-home", "cannot locate the data dir: set ADDITION_HOME or LOCALAPPDATA");
    }
    return path.join(env.LOCALAPPDATA, "addition");
  }
  if (platform === "darwin") {
    return path.join(homedir, "Library", "Application Support", "addition");
  }
  return path.join(env.XDG_DATA_HOME || path.join(homedir, ".local", "share"), "addition");
}
