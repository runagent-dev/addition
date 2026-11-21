# Releasing

A release is a `v*` tag on a commit of `main`. The tag starts `.github/workflows/release.yml`;
nothing else publishes.

## Procedure

1. Merge everything for the release to `main` through the maintainers' usual review. Then confirm the latest
   `main` CI run is green:

   ```bash
   gh run list --repo runagent-dev/addition --branch main --limit 1
   ```

2. An admin tags that exact commit, annotated, and pushes the tag:

   ```bash
   git fetch origin
   git tag -a vX.Y.Z -m "vX.Y.Z" "$(git rev-parse origin/main)"
   git push origin vX.Y.Z
   ```

   The tag must be SemVer 2.0 (`v1.2.3`, `v1.2.3-rc.1`); a tag with a prerelease part is published as a prerelease.
3. Watch the workflow, then run the verification below on the published files.

Never tag an older commit. GitHub runs the workflow definition that exists at the tagged commit, so a tag
on an old commit runs the old workflow with its old permissions and checks. The workflow's own check that the
commit is on `main` only holds for workflows that contain it.

## Rules enforced on GitHub today

Two rulesets are active (`gh api repos/runagent-dev/addition/rulesets`):

- **release tags:** `v*` tags can be created, updated and deleted only by repository admins.
- **main history:** `main` cannot be deleted and cannot be force-pushed (deletion and non-fast-forward rules,
  see `gh api repos/runagent-dev/addition/rules/branches/main`).

Pull request review and passing checks before merging are the maintainers' process; GitHub does not enforce them
yet. That is why step 1 checks the latest `main` run by hand.

## What the workflow does

| Job | Permissions | Does |
|---|---|---|
| `verify` | contents: read | tag matches the SemVer regex, the tagged commit is an ancestor of `origin/main`, gofmt, vet and tests pass |
| `build` | contents: read | pinned GoReleaser builds the tag (`GORELEASER_CURRENT_TAG`), no token; `scripts/check-release-dist.sh` checks the seven assets and that the linux binary reports the tag's version; uploads them as one artifact |
| `attest` | id-token, attestations: write | GitHub build provenance for the six archives and `checksums.txt` |
| `publish` | contents: write | `gh release create` from the same artifact, only after the previous jobs passed |

Builds are reproducible: the same commit gives byte-identical archives (CI builds twice and compares).

## Verifying a release

```bash
gh release download vX.Y.Z --repo runagent-dev/addition --pattern 'addition_linux_amd64.tar.gz' --pattern checksums.txt
sha256sum --ignore-missing -c checksums.txt
gh attestation verify addition_linux_amd64.tar.gz \
  --repo runagent-dev/addition \
  --signer-workflow runagent-dev/addition/.github/workflows/release.yml
```

A release has seven assets: four `tar.gz`, two `zip`, `checksums.txt`. The source archives GitHub adds to
every release are not attested. `install.sh` checks the archive against `checksums.txt` only; it does not
verify the attestation.
