# Contributing to crossplane-helm-postrender

Thank you for your interest in contributing. This document covers development
setup, the checks a change needs to pass, and the conventions the existing
code follows.

## Development setup

### Prerequisites

- **Go 1.26+**: match the version pinned in `go.mod` and
  `.github/workflows/ci.yaml`.
- **Docker**: required to exercise the render engine for real; most unit
  tests substitute the crossplane CLI's `render.MockEngine` and don't need
  it, but anything that actually renders through Function containers does.

### Building

```bash
go build ./...
```

### Running tests

```bash
go test -race ./...
```

`-race` matters here specifically: `BatchRenderer.RenderAll` shares one
engine and one set of running Function containers across streams, and a data
race in that sharing would otherwise only show up under load, not in a
straight-line test run. CI always runs with `-race`; don't drop it locally
just to get a faster loop.

### Linting

```bash
golangci-lint run ./...
```

The lint configuration is `.golangci.yml`: `default: all`, with a short list
of explicit opt-outs, each with a comment explaining why. If a rule genuinely
doesn't fit this project, disable it in `.golangci.yml` with the same kind of
justification -- don't reach for a `//nolint` comment on individual lines
unless the exclusion is truly local to one line (and even then,
`nolintlint` requires an explanation and the specific linter name, not a bare
`//nolint`).

## Code conventions

These aren't arbitrary style preferences -- they're patterns already used
throughout `internal/` and `cmd/`, inherited from
[crossplane-diff](https://github.com/crossplane-contrib/crossplane-diff),
which this project deliberately follows so code reads consistently across the
Crossplane tooling ecosystem.

### Errors

Use `github.com/crossplane/crossplane-runtime/v2/pkg/errors`, not the
standard library's `errors` or `fmt.Errorf`. Messages are lowercase and start
with "cannot `<verb>` `<noun>`":

```go
return nil, errors.Wrap(err, "cannot parse composite resource")
return nil, errors.Wrapf(err, "cannot apply XRD defaults to XR %q", xr.GetName())
return nil, errors.New("no Composition found in input")
```

`errors.Wrap`/`errors.Wrapf` when there's an underlying error to preserve;
`errors.New`/`errors.Errorf` when there isn't. Every wrap message names what
operation failed, not just that something failed -- "cannot parse XRD from
%s" is useful in a log; "unexpected error" is not.

### Tests

Table-driven, keyed by descriptive name (not index), using the standard
library's `testing` package:

```go
tests := map[string]struct {
    in      string
    wantErr string
}{
    "MissingXR": {
        in:      `...`,
        wantErr: "no XR found",
    },
    // ...
}

for name, tt := range tests {
    t.Run(name, func(t *testing.T) {
        // ...
    })
}
```

Use [`google/go-cmp`](https://github.com/google/go-cmp) (`cmp.Diff`) for
structural comparisons where a `-want +got` diff is more useful than a single
`t.Errorf` line, and plain `if got != want { t.Errorf(...) }` checks
elsewhere. **Do not add `testify`** (or `ginkgo`/`gomega`) as a test
dependency -- this is enforced by `depguard` in the upstream
crossplane-diff config this project follows, on the grounds that
assertion libraries obscure what's actually being compared and produce worse
failure messages than the standard library's `t.Errorf` with an explicit
`-want`/`+got`. See <https://go.dev/wiki/TestComments#assert-libraries>.

Test fixtures that need to be realistic multi-document YAML streams live
under `testdata/` (see `internal/parse/testdata/`) rather than as long
string literals, once they'd otherwise dominate a test file.

### Unstructured access for XRDs specifically

`internal/parse/xrd.go` reads XRD fields via `unstructured.NestedString`
rather than decoding into a typed
`apiextensionsv1.CompositeResourceDefinition`. This is deliberate and easy to
accidentally "fix" into a regression: the Kubernetes apiserver round-trips
XRDs through v1↔v2 conversion, so a document's own `apiVersion` field is not
a reliable indicator of which version's schema it actually satisfies. Typed
decoding against a guessed version can silently drop fields that exist under
the other version's shape. Reading the two candidate paths
(`spec.names.kind`, `spec.claimNames.kind`) directly off the unstructured
object sidesteps the whole problem -- there's exactly one place
(`internal/render/engine.go`'s `xrdFrom`) where a typed decode is
unavoidable, because `ApplyXRDDefaults` requires the typed form to derive a
CRD schema, and even there the comment says so explicitly.

If you're adding a new path into an XRD document, follow the same pattern:
unstructured accessors for matching/routing, typed decode only where a
crossplane-cli function genuinely requires it.

### Licensing

Every `.go` file starts with the Apache-2.0 header block already present at
the top of every existing file in this repo. Copy it verbatim into new files;
don't paraphrase it.

## Proposing changes

1. Fork and branch from `main`.
2. Make your change. If it touches render behavior -- anything in
   `internal/render` or `internal/parse`, or how the CLI in
   `cmd/crossplane-postrender` wires them together -- **add a test that would
   fail without the change**. This is the project's actual bar for "done";
   a behavior change with no corresponding test is not reviewable as
   correct, only as plausible.
3. Run `go test -race ./...` and `golangci-lint run ./...` locally before
   opening a PR. Both run in CI (`.github/workflows/ci.yaml`) and will block
   merge on failure, so catching a mismatch locally first saves a
   round-trip.
4. Open a PR describing what changed and why. Reference any related issue.

### A note on chart-testing changes specifically

If you're changing how documents are classified (`internal/parse/parse.go`)
or how inputs are converted before rendering
(`internal/render/engine.go`), consider adding or extending a fixture under
`internal/parse/testdata/` if the change affects how a real `helm template`
or helm-unittest stream is parsed -- the existing fixtures
(`helm-template-stream.yaml`, `helm-v4-plugin-stream.yaml`) exist precisely
to catch regressions in stream-shape handling that a synthetic single-purpose
test string might miss.

## Getting help

Open a [GitHub issue](https://github.com/jcogilvie/crossplane-helm-postrender/issues)
for bugs, feature requests, or questions about intended behavior.
