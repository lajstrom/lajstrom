# Plan sources

`issues/` holds one Markdown file per planned issue, with front matter (`id`, `title`,
`milestone`, `lane`, `size`, `priority`, `labels`, `depends`, optional `assignee`) and a body in
the shape of the Task or Spike issue template. They are the source for the GitHub issues; after
bootstrap the GitHub issue is the live copy and the file is history (do not edit files to track
status).

- `backlog.md` is generated: `tools/bootstrap/bootstrap.py backlog > docs/plan/backlog.md`.
- `tools/bootstrap/bootstrap.py --help` explains how to create labels, milestones, issues and
  the project board from these files, idempotently (`--dry-run` prints the commands).
- Ids: `M<milestone>-<nn>` for scheduled work, `P-<nn>` for post-MVP. The `link` subcommand
  rewrites ids in issue bodies to `#numbers` once the issues exist.
- Adding work during the month: open the issue on GitHub directly with the Task template; the
  files here are for the initial load only.
