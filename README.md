# forksync

Keep a fork of a large upstream (Kubernetes, Kueue, …) current without losing your downstream changes.

The fork's `main` is always **one upstream tag plus an ordered stack of classified patches**. Every upstream release,
a bot re-applies the stack onto the new tag, drops patches upstream has absorbed, and opens a PR you review with
`git range-diff`. Urgent fixes come in as cherry-picks between releases. Downstream releases are tagged
`<upstream tag>-fork.N`.

## Quick start

```sh
go install github.com/helayoty/forksync/cmd/forksync@latest     # or deb / rpm / image, see Install

git clone git@github.com:acme/kubernetes && cd kubernetes
git remote add upstream https://github.com/kubernetes/kubernetes && git fetch --tags upstream
git checkout -b main v1.38.0

forksync init --upstream kubernetes/kubernetes --base v1.38.0 --owner @you \
              --forksync-repo helayoty/forksync --forksync-version v1.2.3 --forksync-sha <that release's commit>
git push -u origin main
```

`init` writes `UPSTREAM_BASE` and four small workflows that call forksync's reusable ones, pinned to a commit.
Then, in the fork's settings:

- Branch protection on `main`: require `patch-policy / policy / lint`; allow merge commits and do not require linear
  history (see [Landing a sync PR](#landing-a-sync-pr)). For your own PRs, prefer squash with the PR title and
  description as the commit message, so the trailers survive.
- Optional secret `FORK_BOT_TOKEN`: a GitHub App or PAT identity **on the fork** (`contents`, `pull-requests`,
  `issues` write). Without it everything runs on `GITHUB_TOKEN`, whose PRs never trigger checks — so the bot's
  sync and cherry-pick PRs would arrive without `patch-policy` or CI. Nothing is needed on forksync itself.

Existing downstream commits must be reworded to the taxonomy below before the first sync; `forksync lint` lists them.

## How it works

### Every downstream commit is classified

| Subject prefix | Trailers | What the sync does with it |
|---|---|---|
| `UPSTREAM: <PR#>: …` | `(cherry picked from commit …)` recommended | Dropped once that PR or sha is in the target tag |
| `UPSTREAM: <carry>: …` | `Downstream-Owner: @who` and `Upstream: <PR url\|#N>` **or** `Upstream: none: <reason>` | PR given → dropped when it lands; `none` → carried forever |
| `UPSTREAM: <drop>: …` | `Downstream-Owner`, `Drop-when: vX.Y.Z` | Dropped at the first sync to that tag or later |

`patch-policy` rejects any PR whose commits don't match. `forksync inventory` prints the live stack; its size is the
cost of the fork, and the `Upstream:` trailer makes "why isn't this upstream yet" answerable.

### Four lanes

| Lane | Runs on | Result |
|---|---|---|
| **sync** | daily | new upstream tag → stack re-applied → `sync/<tag>` PR with range-diff and the list of dropped patches |
| **policy** | every PR | the table above, enforced; inventory posted as a comment |
| **cherry-pick** | `/cherry-pick <PR#> [release-1.NN] [--allow-approved]` comment | upstream PR's commits land as `UPSTREAM: <PR#>:`, auto-dropped at the next sync |
| **release** | manual | lint → tag `<base>-fork.N` with the inventory as notes → GitHub release → build (GitHub or Azure DevOps) |

A patch is recognised as already upstream by: an empty cherry-pick, its `-x` sha being reachable, a release-branch
backport citing that sha, `Merge pull request #N`, `Automated cherry pick of #N` (k/k) or `cherry-pick of #N`
(Prow), or a satisfied `Drop-when`.

### Conflicts

The bot never asks you to resolve on its schedule:

1. Conflicting patches are **skipped**, the rest land, and the PR opens as a draft naming each skipped patch and its owner.
2. The owner runs `git cherry-pick <sha>` on `sync/<tag>`, resolves, pushes. No local setup.
3. The next bot run sees commits it did not make, leaves the branch alone, learns the resolution (`git rerere`, stored
   in the fork's `forksync/rerere` branch) and reuses it on every later sync. Delete the branch to let the bot rebuild it.

### Landing a sync PR

A sync branch is the new upstream tag, the re-applied stack, the `UPSTREAM_BASE` bump, and finally a merge of the old
`main` that keeps the branch's tree and records `Forksync-Supersedes: <old main sha>`. The PR therefore contains its
base, merges without conflicts, and shows the net upgrade diff.

Land it with **Create a merge commit**. Squash or rebase would drop the upstream history the branch brings in.
`sync`, `lint`, `inventory` and `rerere train` find the stack by excluding the upstream tag and every superseded
`main`, so the stack is the same before and after landing, and `patch-policy` passes on the sync PR itself.

### Downstream releases

`release` tags `<UPSTREAM_BASE><sep>fork.<N>`:

| `sep` | Tag | Note |
|---|---|---|
| `-` (default) | `v1.38.0-fork.3` | semver pre-release: sorts *below* `v1.38.0` (Helm `>=` constraints) |
| `+` | `v1.38.0+fork.3` | semver build metadata; image tag becomes `v1.38.0-fork.3` |

Build hand-off, chosen with `init --ado`:

| `--ado` | GitHub `release.yml` | `azure-pipelines.yml` |
|---|---|---|
| *(none)* | tags + release; optional `build-command` | — |
| `trigger` | tags + release | triggers on the tag push |
| `queue` | tags + release, queues ADO via REST (`ADO_ORG`, `ADO_PROJECT`, `ADO_PIPELINE_ID`, `ADO_PAT`) | `trigger: none` |

Both templates leave the actual `make` targets to you.

## Install

| Where | How |
|---|---|
| GitHub Actions | nothing — the reusable workflows install `ghcr.io/helayoty/forksync:<forksync-version>` (cosign-verified), falling back to the signed release asset, then to a source build |
| Azure DevOps | `container: ghcr.io/helayoty/forksync:vX.Y.Z` on the job, or let `build-template.yml` download the asset |
| Any container CI | `ghcr.io/helayoty/forksync:vX.Y.Z` (Debian, `git` + `gh`, no entrypoint) with `command: [forksync, …]` |

Verify a download (keyless cosign; the identity is this repo's `forksync-release.yml` workflow):

```sh
cosign verify ghcr.io/helayoty/forksync:vX.Y.Z \
  --certificate-identity-regexp '^https://github.com/helayoty/forksync/\.github/workflows/forksync-release\.yml@refs/(heads/main|tags/v[0-9].*)$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
cosign verify-blob --bundle checksums.txt.sigstore.json <same two flags> checksums.txt && sha256sum -c --ignore-missing checksums.txt
gh attestation verify forksync_linux_amd64.tar.gz --repo helayoty/forksync
```

## CLI

```
forksync init          --upstream o/r --base vX.Y.Z --owner @me --forksync-repo o/forksync --forksync-sha <sha> [--forksync-version vX.Y.Z] [--ado trigger|queue]
forksync sync          --ref vX.Y.Z [--head main] [--report sync.md]        exit 2 = conflicts
forksync lint          --base origin/main [--head HEAD]
forksync inventory
forksync cherry-pick   --repo o/r --pr N --owner @me [--release-branch release-1.NN] [--backport-style k8s|prow] [--allow-approved]
forksync latest-tag    [--pattern 'v*']          forksync next-tag [--sep -|+]
forksync rerere        pull | push | train --sync-branch origin/sync/vX.Y.Z
forksync version
```

## Maintaining forksync

`forksync-release.yml` is internal to this repo (dispatch/tag only, no `workflow_call`); forks never reference it —
their own `release.yml` calls `reusable-release.yml`.

- **Release**: Actions → *forksync-release* → *Run workflow* → pick `patch` / `minor` / `major` (or type a version). The run
  tests, tags `vX.Y.Z`, then publishes: multi-arch
  image, archives, deb/rpm, SBOMs, cosign signatures, SLSA attestations. The release notes end with the exact `init` flags consumers should pin.
- After the **first** release, make the ghcr package public (packages default to private).
- `go test ./...` builds real git fixtures: every drop signal, patch-release backports, conflict skipping, rerere
  train → push → pull, `init` in all ADO modes. `make image` builds a local snapshot without publishing.
- Breaking changes to workflow inputs → major bump; consumers re-run `init` or edit the pinned sha in four files.
