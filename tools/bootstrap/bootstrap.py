#!/usr/bin/env python3
"""Bootstrap the GitHub project tracking for Lajstrom from docs/plan/issues/*.md.

Idempotent: labels and milestones are created or updated by name, issues are matched by title.
Needs the GitHub CLI (`gh`) logged in with repo scope. Nothing here changes repository settings
unless you run the `settings` subcommand explicitly.

Usage:
  tools/bootstrap/bootstrap.py labels            [--dry-run]
  tools/bootstrap/bootstrap.py milestones        [--dry-run]
  tools/bootstrap/bootstrap.py issues [--milestone M0] [--dry-run]
  tools/bootstrap/bootstrap.py link              [--dry-run]   # rewrite M0-02 -> #12 in bodies
  tools/bootstrap/bootstrap.py backlog > docs/plan/backlog.md  # regenerate the overview
  tools/bootstrap/bootstrap.py project           [--dry-run]   # needs: gh auth refresh -s project
  tools/bootstrap/bootstrap.py settings          [--dry-run]   # discussions, required reviews
  tools/bootstrap/bootstrap.py all               [--dry-run]   # labels, milestones, issues, link

Environment: LAJSTROM_REPO (default lajstrom/lajstrom).
"""
from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
from pathlib import Path

REPO = os.environ.get("LAJSTROM_REPO", "lajstrom/lajstrom")
ROOT = Path(__file__).resolve().parents[2]
ISSUES_DIR = ROOT / "docs" / "plan" / "issues"
ID_RE = re.compile(r"\b(M[0-3]-\d{2}|P-\d{2})\b")

LABELS = {
    # area
    "area/core": ("0e8a16", "Server core: cmd, config, server, ingest, keys"),
    "area/facade": ("0e8a16", "OCI facade /v2"),
    "area/provider": ("0e8a16", "Registry provider and adapters"),
    "area/index": ("0e8a16", "Index DB, migrations, reindex"),
    "area/formats": ("1d76db", "Format plugins and protocol frontends"),
    "area/auth": ("5319e7", "Authentication, tokens, RBAC"),
    "area/api": ("5319e7", "Management API /api/v1 and admin CLI"),
    "area/ci": ("fbca04", "CI, integration and e2e harness"),
    "area/docs": ("0075ca", "Documentation"),
    "area/release": ("fbca04", "Release engineering, images, packaging"),
    "area/dev": ("fbca04", "Developer environment"),
    # kind
    "kind/feature": ("a2eeef", "New behaviour"),
    "kind/bug": ("d73a4a", "Something is broken"),
    "kind/spike": ("bfd4f2", "Time-boxed investigation with a written result"),
    "kind/chore": ("ededed", "Maintenance, tooling, housekeeping"),
    "kind/docs": ("0075ca", "Documentation work"),
    "kind/test": ("c5def5", "Tests and test infrastructure"),
    # lane
    "lane/core": ("006b75", "Core lane (lead): cmd, server, provider, index, ingest, facade, auth, api"),
    "lane/formats": ("006b75", "Formats lane: oci/profile, format/*, fixtures"),
    "lane/platform": ("006b75", "Platform lane: dev stack, CI, e2e, docs, release, spikes"),
    # size
    "size/S": ("e4e669", "Up to half a day"),
    "size/M": ("e4e669", "One to two days"),
    "size/L": ("e4e669", "Three to five days; split if it grows"),
    # priority
    "priority/p0": ("b60205", "Defines the milestone"),
    "priority/p1": ("d93f0b", "Planned; cut first if the week runs out"),
    "priority/p2": ("fef2c0", "Opportunistic"),
    # flags
    "contract": ("000000", "Touches a documented interface or spec; needs lead approval"),
    "post-mvp": ("cfd3d7", "Not scheduled before v0.1.0"),
    "blocked": ("d73a4a", "Cannot proceed; see the last comment"),
    "needs-decision": ("d876e3", "Waiting for a maintainer decision"),
    "good first issue": ("7057ff", "Good for newcomers"),
    "help wanted": ("008672", "Extra attention is needed"),
}

MILESTONES = {
    "M0": ("M0 — Skeleton and raw end-to-end", "2026-11-13",
           "make dev-up && make run; curl -T a file into /raw and curl it back; manifest visible in Zot; "
           "interfaces merged; CI green on SQLite and Postgres. See docs/roadmap.md"),
    "M1": ("M1 — Facade, reindex, pypi", "2026-11-20",
           "oras push through the facade with a token; twine upload + pip install; reindex proven by test; "
           "cosign through the facade; registry matrix from spikes. See docs/roadmap.md"),
    "M2": ("M2 — apt and auth", "2026-11-27",
           "apt-get update/install from a signed repo; users, PATs, deploy tokens, RBAC bindings, anonymous read, "
           "audit; /api/v1 and admin CLI. See docs/roadmap.md"),
    "M3": ("M3 — maven and v0.1.0", "2026-12-04",
           "mvn deploy + dependency:get; goreleaser artifacts, images, SBOM, cosign; install docs; tag v0.1.0. "
           "See docs/roadmap.md"),
}


def sh(args: list[str], dry: bool = False, check: bool = True, input_: str | None = None) -> str:
    if dry:
        print("+ " + " ".join(repr(a) if (" " in a or "\n" in a) else a for a in args)[:300])
        return ""
    r = subprocess.run(args, capture_output=True, text=True, input=input_)
    if check and r.returncode != 0:
        sys.stderr.write(r.stderr)
        raise SystemExit(f"command failed: {' '.join(args[:4])} ...")
    return r.stdout


def gh_api(path: str, method: str = "GET", fields: dict | None = None, dry: bool = False, paginate: bool = False):
    args = ["gh", "api"]
    if paginate:
        args.append("--paginate")
    if method != "GET":
        args += ["-X", method]
    args.append(path)
    body = None
    if fields is not None:
        args += ["--input", "-"]
        body = json.dumps(fields)
    out = sh(args, dry=dry, input_=body)
    if dry or not out.strip():
        return None
    try:
        return json.loads(out)
    except json.JSONDecodeError:
        # --paginate concatenates JSON arrays
        items = []
        for chunk in re.split(r"\]\s*\[", out.strip()):
            chunk = chunk if chunk.startswith("[") else "[" + chunk
            chunk = chunk if chunk.endswith("]") else chunk + "]"
            items += json.loads(chunk)
        return items


def parse_issue(path: Path) -> dict:
    text = path.read_text(encoding="utf-8")
    m = re.match(r"^---\n(.*?)\n---\n(.*)$", text, re.S)
    if not m:
        raise SystemExit(f"{path}: missing front matter")
    fm, body = m.group(1), m.group(2).strip() + "\n"
    meta: dict = {}
    for line in fm.splitlines():
        if not line.strip() or ":" not in line:
            continue
        k, v = line.split(":", 1)
        v = v.strip()
        if v.startswith("[") and v.endswith("]"):
            v = [x.strip() for x in v[1:-1].split(",") if x.strip()]
        elif v.startswith('"') and v.endswith('"'):
            v = v[1:-1]
        meta[k.strip()] = v
    for key in ("id", "title", "milestone"):
        if key not in meta:
            raise SystemExit(f"{path}: front matter lacks {key}")
    meta.setdefault("depends", [])
    meta.setdefault("labels", [])
    meta["body"] = body
    meta["file"] = path.name
    labels = list(meta["labels"])
    for key in ("lane", "size", "priority"):
        val = meta.get(key)
        if val and val != "none":
            labels.append(f"{key}/{val}")
    meta["all_labels"] = sorted(set(labels))
    return meta


def load_issues() -> list[dict]:
    issues = [parse_issue(p) for p in sorted(ISSUES_DIR.glob("*.md"))]
    ids = [i["id"] for i in issues]
    dupes = {x for x in ids if ids.count(x) > 1}
    if dupes:
        raise SystemExit(f"duplicate ids: {dupes}")
    known = set(ids)
    for i in issues:
        for d in i["depends"]:
            if d not in known:
                raise SystemExit(f"{i['id']} depends on unknown {d}")
        for l in i["all_labels"]:
            if l not in LABELS:
                raise SystemExit(f"{i['id']} uses unknown label {l}")
    return issues


def cmd_labels(dry: bool):
    existing = {l["name"]: l for l in (gh_api(f"repos/{REPO}/labels?per_page=100", paginate=True) or [])}
    for name, (color, desc) in LABELS.items():
        if name in existing:
            cur = existing[name]
            if cur.get("color") == color and (cur.get("description") or "") == desc:
                continue
            sh(["gh", "label", "edit", name, "--repo", REPO, "--color", color, "--description", desc], dry=dry)
            print(f"updated label {name}")
        else:
            sh(["gh", "label", "create", name, "--repo", REPO, "--color", color, "--description", desc], dry=dry)
            print(f"created label {name}")


def milestone_map(dry: bool) -> dict[str, dict]:
    if dry:
        return {}
    ms = gh_api(f"repos/{REPO}/milestones?state=all&per_page=100", paginate=True) or []
    return {m["title"]: m for m in ms}


def cmd_milestones(dry: bool):
    existing = milestone_map(dry)
    for key, (title, due, desc) in MILESTONES.items():
        fields = {"title": title, "due_on": f"{due}T23:59:59Z", "description": desc, "state": "open"}
        if title in existing:
            num = existing[title]["number"]
            gh_api(f"repos/{REPO}/milestones/{num}", "PATCH", fields, dry=dry)
            print(f"updated milestone {title}")
        else:
            gh_api(f"repos/{REPO}/milestones", "POST", fields, dry=dry)
            print(f"created milestone {title}")


def existing_issues() -> dict[str, dict]:
    out = sh(["gh", "issue", "list", "--repo", REPO, "--state", "all", "--limit", "500",
              "--json", "number,title,body,labels,milestone,assignees"])
    return {i["title"]: i for i in json.loads(out)}


def issue_body(meta: dict, idmap: dict[str, int]) -> str:
    body = meta["body"]
    deps = meta["depends"]
    footer = [f"\n---\n_Plan id: `{meta['id']}` · source: `docs/plan/issues/{meta['file']}`_\n"]
    if deps:
        footer.insert(0, "\n**Depends on:** " + ", ".join(deps) + "\n")
    body = body + "".join(footer)

    def repl(m):
        pid = m.group(1)
        return f"#{idmap[pid]} ({pid})" if pid in idmap and pid != meta["id"] else pid
    return ID_RE.sub(repl, body)


def cmd_issues(dry: bool, only_milestone: str | None):
    issues = load_issues()
    existing = {} if dry else existing_issues()
    ms = milestone_map(dry)
    created = 0
    for meta in issues:
        if only_milestone and meta["milestone"] != only_milestone:
            continue
        title = meta["title"]
        if title in existing:
            print(f"exists  #{existing[title]['number']} {title}")
            continue
        args = ["gh", "issue", "create", "--repo", REPO, "--title", title, "--body", issue_body(meta, {})]
        for l in meta["all_labels"]:
            args += ["--label", l]
        mkey = meta["milestone"]
        if mkey != "none":
            mtitle = MILESTONES[mkey][0]
            if not dry and mtitle not in ms:
                raise SystemExit(f"milestone {mtitle} missing; run `milestones` first")
            args += ["--milestone", mtitle]
        if meta.get("assignee"):
            args += ["--assignee", meta["assignee"]]
        out = sh(args, dry=dry)
        created += 1
        print(f"created {out.strip() or title}")
    print(f"{created} created")
    if created and not dry:
        print("now run: bootstrap.py link   (rewrites plan ids to #numbers in bodies)")


def cmd_link(dry: bool):
    issues = load_issues()
    existing = existing_issues()
    idmap = {m["id"]: existing[m["title"]]["number"] for m in issues if m["title"] in existing}
    for meta in issues:
        gh_issue = existing.get(meta["title"])
        if not gh_issue:
            continue
        new_body = issue_body(meta, idmap)
        if (gh_issue.get("body") or "").strip() == new_body.strip():
            continue
        sh(["gh", "issue", "edit", str(gh_issue["number"]), "--repo", REPO, "--body", new_body], dry=dry)
        print(f"linked #{gh_issue['number']} {meta['id']}")


def cmd_backlog():
    issues = load_issues()
    print("# Backlog overview\n")
    print("Generated by `tools/bootstrap/bootstrap.py backlog` from `docs/plan/issues/`. Do not edit by hand;")
    print("after bootstrap the GitHub issues are the live status.\n")
    for mkey in ("M0", "M1", "M2", "M3", "none"):
        rows = [i for i in issues if i["milestone"] == mkey]
        if not rows:
            continue
        if mkey in MILESTONES:
            title = f"{MILESTONES[mkey][0]} (due {MILESTONES[mkey][1]})"
        else:
            title = "Post-MVP (no milestone)"
        print(f"## {title}\n")
        print("| id | title | lane | size | prio | depends on |")
        print("| --- | --- | --- | --- | --- | --- |")
        for i in sorted(rows, key=lambda x: x["id"]):
            print(f"| {i['id']} | {i['title']} | {i.get('lane', '')} | {i.get('size', '')} | "
                  f"{i.get('priority', '')} | {', '.join(i['depends'])} |")
        print()
    sizes = {"S": 0.5, "M": 1.5, "L": 4}
    tot: dict = {}
    for i in issues:
        if i["milestone"] in MILESTONES:
            key = (i["milestone"], i.get("priority"))
            tot[key] = tot.get(key, 0) + sizes.get(i.get("size"), 0)
    print("## Rough effort (person-days with AI assistance; S=0.5, M=1.5, L=4)\n")
    print("| milestone | p0 | p1 | p2 | total |")
    print("| --- | --- | --- | --- | --- |")
    for mkey in ("M0", "M1", "M2", "M3"):
        p0, p1, p2 = (tot.get((mkey, p), 0) for p in ("p0", "p1", "p2"))
        print(f"| {mkey} | {p0:g} | {p1:g} | {p2:g} | {p0 + p1 + p2:g} |")


def cmd_project(dry: bool):
    owner = REPO.split("/")[0]
    out = sh(["gh", "project", "list", "--owner", owner, "--format", "json"], dry=dry, check=False)
    number = None
    if out:
        for p in json.loads(out).get("projects", []):
            if p["title"] == "Lajstrom MVP":
                number = p["number"]
    if number is None:
        out = sh(["gh", "project", "create", "--owner", owner, "--title", "Lajstrom MVP", "--format", "json"], dry=dry)
        number = json.loads(out)["number"] if out else "<n>"
        print(f"created project {number}")
    existing = {} if dry else existing_issues()
    for meta in load_issues():
        gi = existing.get(meta["title"])
        if gi:
            sh(["gh", "project", "item-add", str(number), "--owner", owner, "--url",
                f"https://github.com/{REPO}/issues/{gi['number']}"], dry=dry, check=False)
    print("Set up the board columns by hand: Backlog, Ready for Development, In Progress, In review, Done; "
          "enable the built-in workflows (item added -> Backlog, PR merged -> Done).")


def cmd_settings(dry: bool):
    print("Enabling Discussions and requiring 1 review on main (explicit maintainer action).")
    gh_api(f"repos/{REPO}", "PATCH", {"has_discussions": True}, dry=dry)
    prot = gh_api(f"repos/{REPO}/branches/main/protection", dry=dry) or {}
    rsc = prot.get("required_status_checks") or {}
    checks = [c["context"] for c in rsc.get("checks", [])]
    # Code-owner reviews are not required: the lead is the only code owner of the specs and
    # cannot approve their own pull requests. CODEOWNERS still auto-requests the review.
    fields = {
        "required_status_checks": {"strict": bool(rsc.get("strict", False)), "contexts": checks},
        "enforce_admins": True,
        "required_pull_request_reviews": {"required_approving_review_count": 1, "dismiss_stale_reviews": True,
                                          "require_code_owner_reviews": False},
        "restrictions": None,
        "required_linear_history": True,
        "allow_force_pushes": False,
        "allow_deletions": False,
    }
    gh_api(f"repos/{REPO}/branches/main/protection", "PUT", fields, dry=dry)
    print("Discussion categories (Weekly, Design, Q&A, Announcements) must be created in the web UI.")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("command", choices=["labels", "milestones", "issues", "link", "backlog", "project", "settings", "all"])
    ap.add_argument("--milestone", help="only this milestone key (M0..M3, none)")
    ap.add_argument("--dry-run", action="store_true")
    a = ap.parse_args()
    if a.command == "labels":
        cmd_labels(a.dry_run)
    elif a.command == "milestones":
        cmd_milestones(a.dry_run)
    elif a.command == "issues":
        cmd_issues(a.dry_run, a.milestone)
    elif a.command == "link":
        cmd_link(a.dry_run)
    elif a.command == "backlog":
        cmd_backlog()
    elif a.command == "project":
        cmd_project(a.dry_run)
    elif a.command == "settings":
        cmd_settings(a.dry_run)
    elif a.command == "all":
        cmd_labels(a.dry_run)
        cmd_milestones(a.dry_run)
        cmd_issues(a.dry_run, a.milestone)
        if not a.dry_run:
            cmd_link(False)


if __name__ == "__main__":
    main()
