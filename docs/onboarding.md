# Start here

You are reading this because you want to contribute to Lajstrom. The path below takes about an
hour and ends with you working on an issue.

## 1. Read (30 minutes)

| Order | What | Why |
| --- | --- | --- |
| 1 | [`README.md`](../README.md) | what Lajstrom is, in one screen |
| 2 | [`docs/architecture.md`](architecture.md) §Invariants, §1, §2, §3 | the model: registry is truth, DB is derived, one write path |
| 3 | [`docs/spec/manifest-profile.md`](spec/manifest-profile.md) | what every artifact looks like in the registry; you will need it whatever you work on |
| 4 | [`docs/spec/layout.md`](spec/layout.md) | where code goes, the interfaces, the approved dependencies, how tests are organised |
| 5 | [`docs/roadmap.md`](roadmap.md) | this month's milestones and lanes |
| 6 | [`docs/process.md`](process.md) | how an issue becomes a merged pull request |
| 7 | [`CONTRIBUTING.md`](../CONTRIBUTING.md) | DCO sign-off, CLA, `Assisted-by` trailer, license allowlist |

Skim later, as needed: `docs/spec/http-routing.md`, `docs/spec/index-schema.md`,
`docs/spec/rbac.md`, the ADRs in `docs/adr/`.

## 2. Set up (15 minutes)

```sh
# tools
go version            # current stable (CI uses "stable"); 1.23+ works
docker compose version
oras version          # https://oras.land, for pushing test artifacts
gh --version          # GitHub CLI, for issues and pull requests

git clone git@github.com:lajstrom/lajstrom.git && cd lajstrom
make dev-up           # Zot, distribution and Postgres in containers (after M0-09 lands)
make check            # build, vet, test, lint, vulncheck, licenses: what CI runs
```

Until the skeleton (M0-01) and the compose stack (M0-09) are merged, run the Go commands from
CONTRIBUTING.md §Building and testing locally directly.

Set the commit trailers once so you never forget them:

```sh
git config user.name "Your Name"
git config user.email "you@example.com"
# every commit: git commit -s  (adds Signed-off-by)
# commits with AI help: add a trailer line  Assisted-by: Claude Code
```

## 3. Pick an issue (5 minutes)

```sh
gh issue list --milestone "M0 — Skeleton and raw end-to-end" --label "good first issue"
gh issue list --milestone "M0 — Skeleton and raw end-to-end" --label "lane/formats" --state open
```

Pick one that is not assigned and has no open `Depends on` items, assign yourself
(`gh issue edit <n> --add-assignee @me`), and say hello in the issue with a five-line plan if it
is `size/M` or larger. Then branch, open a draft pull request, and go (process.md §3).

## 4. If you use Claude Code

The repository has a `CLAUDE.md`; Claude Code reads it automatically. A good first message:

```text
Read CLAUDE.md, docs/architecture.md and the specs it points to. Then read issue #<n>
(gh issue view <n>). Propose a plan: files to touch, tests to write, open questions.
Do not write code until I confirm the plan.
```

The `.claude/settings.json` in the repository pre-approves the build and test commands so
the loop is fast. It never approves `git push`; you push.

## 5. Ask

- A question about the design: the *Design* discussion, or a comment on the issue.
- Something is broken in `main`: open a `kind/bug` issue with the command and output.
- You are stuck for more than an hour: say so in the chat or the issue; someone will pair.
