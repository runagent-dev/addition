import { ShimError } from "./errors.mjs";

const OS = { linux: "linux", darwin: "darwin", win32: "windows" };
const ARCH = { x64: "amd64", arm64: "arm64" };

/** The release archive for a Node platform and CPU, named as GoReleaser names them, and the binary inside it. */
export function assetFor(platform, arch) {
  const os = OS[platform];
  const cpu = ARCH[arch];
  if (!os || !cpu) {
    const supported = Object.keys(OS).flatMap((p) => Object.keys(ARCH).map((a) => `${p}-${a}`));
    throw new ShimError(
      "unsupported-platform",
      `no addition release exists for ${platform}-${arch} (supported: ${supported.join(", ")}). ` +
        "Build from source with `go install github.com/runagent-dev/addition/cmd/addition@latest`.",
    );
  }
  const windows = platform === "win32";
  return {
    archive: `addition_${os}_${cpu}.${windows ? "zip" : "tar.gz"}`,
    binary: windows ? "addition.exe" : "addition",
  };
}
