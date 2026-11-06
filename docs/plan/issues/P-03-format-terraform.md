---
id: P-03
title: "feat(format/terraform): terraform module registry protocol and .well-known discovery"
milestone: none
lane: none
size: L
priority: p2
labels: [area/formats, kind/feature, post-mvp]
depends: []
---
**Goal**

`/.well-known/terraform.json`, `/<ns>/<name>/<provider>/versions` and `download` endpoints with `X-Terraform-Get`; namespace = repo (FQDN mode or host-level discovery). OpenTofu `oci://` native path documented as the alternative.

**Status**

Post-MVP. Not scheduled; discussed at the Friday review once v0.1.0 is out. Comments and design
notes welcome; no implementation PRs before it gets a milestone.
