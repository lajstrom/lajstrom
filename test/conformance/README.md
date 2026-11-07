# OCI distribution-spec conformance

`run.sh` runs the conformance suite of the OCI distribution spec against the facade of a running
Lajstrom, on a repo of format `oci` (pass-through: the facade stores what the suite pushes and
indexes nothing). The suite is a Go module of its own,
`github.com/opencontainers/distribution-spec/conformance`; the script clones it at a release tag
and builds it outside Lajstrom's module, which gains no dependency.

## Run it

```sh
make dev-up
make run                                              # Lajstrom on :8080, Zot as "local"
bin/lajstrom admin repo create oci/conformance --registry local --config deploy/compose/lajstrom.example.yaml
test/conformance/run.sh
```

The defaults are those of issue #24:

| Variable | Default |
| --- | --- |
| `OCI_ROOT_URL` | `http://localhost:8080` |
| `OCI_NAMESPACE` | `oci/conformance/a` |
| `OCI_CROSSMOUNT_NAMESPACE` | `oci/conformance/b` |
| `OCI_TEST_PULL`, `OCI_TEST_PUSH`, `OCI_TEST_CONTENT_DISCOVERY` | `1` |
| `OCI_TEST_CONTENT_MANAGEMENT` | `0` (deletes come with #61) |
| `OCI_HIDE_SKIPPED_WORKFLOWS` | `1` |
| `SPEC_VERSION` | `v1.1.1`, the tag of the suite |
| `CONFORMANCE_DIR` | `~/.cache/lajstrom/conformance`: the clone and the built `conformance.test` |
| `OCI_REPORT_DIR` | `$CONFORMANCE_DIR/report`: `junit.xml` and `report.html` |

Any of them set in the environment wins; arguments go to the suite (ginkgo flags, e.g.
`-ginkgo.focus='Push'`). Against the distribution of the stack, make a repo on the registry
`second` and point the namespaces at it:

```sh
bin/lajstrom admin repo create oci/conformance-second --registry second --config deploy/compose/lajstrom.example.yaml
OCI_NAMESPACE=oci/conformance-second/a OCI_CROSSMOUNT_NAMESPACE=oci/conformance-second/b test/conformance/run.sh
```

## What passes

Measured on 2026-11-07 with v1.1.1 against Zot and against distribution: **pull** (23 specs)
and **push** (26 specs) pass. **Content discovery** passes its tag listing and fails five specs
until the referrers API of #28: the four `GET …/referrers/<digest>` specs (the facade answers
`501 UNSUPPORTED`) and the references setup, which wants `OCI-Subject` on the `PUT` of a
manifest with a subject; the pass-through does not see the backend's response headers. The
suite exits non-zero for those five; the pull and push lines of its summary are the ones to
read.
