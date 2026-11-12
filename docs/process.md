# How we work

- Status: in force from 2026-11-09. Changes by pull request; the lead decides on objections.
- Who: a team of two plus anyone who shows up, all working with AI coding
  assistants. This page is what keeps that from turning into diverging code bases.

## 1. Where things are

| Thing | Place |
| --- | --- |
| Tasks, status, assignment | GitHub Issues with milestones (M0 onward; [roadmap.md](roadmap.md) has the plan) and the labels below. Nothing is "in progress" unless an issue says so. |
| Board | GitHub Project "Lajstrom MVP" (https://github.com/orgs/lajstrom/projects/1), columns below, auto-synced from issues and pull requests |
| Design truth | `docs/architecture.md`, `docs/spec/*.md`, `docs/adr/*.md` |
| Plan | `docs/roadmap.md` (narrative), milestones (dates), `docs/plan/` (issue sources) |
| Decisions | ADRs for architecture; the roadmap's decision log for scope; issue comments for everything local |
| Async discussion | GitHub Discussions: *Weekly* (plans and demos), *Design*, *Q&A*, *Announcements* |
| Quick questions | no chat for now: a comment on the issue, or the *Q&A* discussion. A chat is added when the team grows; anything that matters is copied back to an issue the same day. |

Board columns (the `Status` field of the project):

| Column | Meaning | Who moves it |
| --- | --- | --- |
| Backlog | filed; a dependency is still open, or it is not this week's work | automation on creation |
| Ready for Development | every `Depends on` item is merged; unassigned, pick it up | the lead, on Monday |
| In Progress | someone is assigned and working on it; a draft PR is expected within a day | the person who took it |
| In Review | a pull request is open with green CI, waiting for one approval | the person who took it |
| Done | the pull request is merged and the issue closed | automation on merge |

## 2. Labels

| Group | Labels | Meaning |
| --- | --- | --- |
| area | `area/core`, `area/facade`, `area/provider`, `area/index`, `area/formats`, `area/auth`, `area/api`, `area/ci`, `area/docs`, `area/release`, `area/dev` | which part of the code |
| kind | `kind/feature`, `kind/bug`, `kind/spike`, `kind/chore`, `kind/docs`, `kind/test` | what kind of work |
| lane | `lane/core`, `lane/formats`, `lane/platform` | who normally takes it (roadmap §Lanes) |
| size | `size/S` (≤ half a day), `size/M` (1–2 days), `size/L` (3–5 days) | with AI assistance; an L that grows is split |
| priority | `priority/p0` (defines the milestone), `priority/p1` (planned, cut first), `priority/p2` (opportunistic) | |
| flags | `contract`, `post-mvp`, `blocked`, `needs-decision`, `good first issue`, `help wanted` | `contract`: touches a documented interface or spec, needs lead approval |

## 3. The loop for one issue

1. **Pick** an issue from the current milestone, in your lane, `Ready for Development` on the board. Newcomers:
   `good first issue`. Do not start an issue that is `blocked` or whose `Depends on` list has
   open items.
2. **Claim** it: assign yourself and move it to *In progress*. One person per issue; at most two
   issues in progress per person. If you stop working on it for more than two days, unassign
   yourself and say why.
3. **Plan in the issue** for `size/M` and `size/L`: a comment with the files you will touch, the
   tests you will write and any question for the lead. Five lines is enough. For `contract`
   issues wait for the lead's thumbs-up on the plan before writing code.
4. **Branch** from `main`: `<type>/<issue-number>-<short-slug>` (`feat/23-pypi-simple-index`).
5. **Open a draft pull request on the first day**, even with one commit. The draft is how the
   others see what you are doing; it links the issue (`Closes #23`).
6. **Work in small commits**, each signed off (`git commit -s`) and, when an assistant wrote
   code, with `Assisted-by: <tool>` (CONTRIBUTING.md). Run `make check` before every push.
7. **Ready for review** when the acceptance criteria of the issue are checked off in the pull
   request description and CI is green. Keep the diff under ~400 lines where you can; split
   otherwise. Add the demo command to the description.
8. **Review**: one approval by a maintainer; a pull request labelled `contract` needs the lead's.
   Reviews are answered within one working day; a reviewer who cannot make it says so in the
   thread. Review the behaviour and the tests, not the style (lint does style).
9. **Merge**: a maintainer merges after that approval, with squash or rebase (linear history).
   The author does not merge their own pull request, and an AI assistant (Claude Code or any
   other) never merges. Delete the branch. Close the issue if the pull request did not.
10. **Demo on Friday**: a short transcript (commands and output) in the *Weekly* discussion
    thread of the milestone.

## 4. Definition of Done

A pull request is done when:

- the issue's acceptance criteria are met and ticked in the description;
- unit tests cover the new behaviour; integration or e2e tests when it touches the registry,
  the database, or a protocol endpoint;
- `make check` passes locally and CI is green on SQLite and Postgres;
- docs are updated when a user-facing behaviour, config key, endpoint or command changed;
- no new dependency outside `docs/spec/layout.md` §3, or the exception is written in the PR;
- the commit trailers are there (DCO, `Assisted-by`), and the description is written by a human.

## 5. Contracts and design changes

`docs/spec/*.md`, `docs/adr/*.md`, `CLAUDE.md` and the exported interfaces of `internal/format`,
`internal/provider`, `internal/index`, `internal/ingest` are **contracts**. To change one:

1. open an issue with the `contract` label (or add it), describing the change and who it
   affects;
2. the lead agrees in the issue (or asks for an ADR when the change is architectural);
3. the spec and the code change in the same pull request, reviewed by the lead;
4. the change is announced in the *Weekly* thread so the other lanes rebase.

A pull request that silently changes a contract is sent back, however good the code is.

## 6. Cadence

| When | What |
| --- | --- |
| Monday, async | each person posts in the milestone's *Weekly* thread: the issues they take this week, blockers |
| Daily, optional | one line in the milestone's *Weekly* thread: what moved, what is stuck |
| Friday, async | demos (transcripts) in the *Weekly* thread; the lead posts milestone status and the cut list for the next week |
| Friday 30 min, optional call | only if a decision is blocked in writing |
| Milestone end | the lead closes the milestone, moves leftovers, updates `docs/roadmap.md` if a date changes |

## 7. Working with an AI assistant

Everyone here uses one. The rules that make it work in a shared code base:

- **The repository's `CLAUDE.md` is loaded by Claude Code automatically.** Keep it current; it
  is the assistant's onboarding. Other assistants: paste it as the first message.
- **Start every session with the issue**, not with a vague goal: "Implement #23 following
  CLAUDE.md, docs/spec/http-routing.md §pypi and docs/spec/manifest-profile.md. Plan first."
- **Plan mode first** for anything `size/M` or larger; read the plan before letting it code.
- **Never let the assistant edit contracts** (`docs/spec`, `docs/adr`, exported interfaces) as
  part of a feature; if it says the interface must change, stop and open the `contract` issue.
- **Tests are not optional for generated code.** Ask for the test first when in doubt.
- **No code from other projects.** Reference implementations (Gitea, distribution, oras-go) are
  for reading; the snippet scan in CI will flag copies.
- **You own the pull request.** Read the whole diff before asking for review. "The tool wrote
  it" is not an answer in review (CONTRIBUTING.md).
- **One issue per conversation** where possible; long sessions drift into other lanes.
- **Disclose**: `Assisted-by: Claude Code` (or the tool you used) on every commit it touched.

## 8. Maintainers' duties this month

- Lead (@gecube): keeps `main` releasable, approves `contract` changes, triages new issues
  within a day, posts Friday status, owns M0-01, M3-08.
- Every maintainer: reviews within one working day, keeps their lane's issues up to date,
  writes the issue for anything they discover instead of fixing it silently.
