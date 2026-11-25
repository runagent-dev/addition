import assert from "node:assert/strict";
import { test } from "node:test";

import { assetFor } from "../lib/platform.mjs";

test("every supported platform maps to the release archive GoReleaser writes", () => {
  const cases = [
    ["linux", "x64", "addition_linux_amd64.tar.gz", "addition"],
    ["linux", "arm64", "addition_linux_arm64.tar.gz", "addition"],
    ["darwin", "x64", "addition_darwin_amd64.tar.gz", "addition"],
    ["darwin", "arm64", "addition_darwin_arm64.tar.gz", "addition"],
    ["win32", "x64", "addition_windows_amd64.zip", "addition.exe"],
    ["win32", "arm64", "addition_windows_arm64.zip", "addition.exe"],
  ];
  for (const [platform, arch, archive, binary] of cases) {
    assert.deepEqual(assetFor(platform, arch), { archive, binary }, `${platform}-${arch}`);
  }
});

test("an unsupported platform is named in the error with the ones that work", () => {
  assert.throws(() => assetFor("freebsd", "x64"), (error) => {
    assert.equal(error.code, "unsupported-platform");
    assert.match(error.message, /freebsd-x64/);
    assert.match(error.message, /linux-x64/);
    return true;
  });
  assert.throws(() => assetFor("linux", "ia32"), { code: "unsupported-platform" });
});
