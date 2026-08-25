# crossplane-helm-postrender

A Helm post-renderer that renders Crossplane compositions, so charts can be
unit-tested against the managed resources they actually produce.

## Overview

A Helm chart that ships Crossplane compositions usually only gets tested
against the Composite Resources (XRs) it templates. That leaves the
composition pipeline itself unverified: the actual `Bucket`, `Instance`,
`RolePolicyAttachment`, or whatever the composition produces never appears in
a test assertion, because `helm template` and `helm unittest` stop at the XR.

`crossplane-postrender` closes that gap. It is a Helm post-renderer -- a
program that reads a rendered manifest stream on stdin and writes a modified
stream to stdout, per [Helm's post-rendering contract][helm-postrender]. This
one takes the XR, Composition, and Function packages a chart renders, feeds
them through the same render engine Crossplane itself uses, and writes the
*composed* resources to stdout in their place. The result is that a chart's
tests can assert directly on `spec.forProvider.region` of a rendered `Bucket`,
not just on the XR fields that are supposed to produce it.

It pairs naturally with [helm-unittest][helm-unittest], which supports an
arbitrary post-renderer per test suite.

[helm-postrender]: https://helm.sh/docs/topics/advanced/#post-rendering
[helm-unittest]: https://github.com/helm-unittest/helm-unittest

## Installation

```bash
go install github.com/jcogilvie/crossplane-helm-postrender/cmd/crossplane-postrender@latest
```

Prebuilt release binaries and an `install.sh` script are also on the way; once
published, they'll be linked from this section. Until then, `go install` (or a
manual `go build ./cmd/crossplane-postrender`) is the supported path.

## Prerequisites

- **Docker.** The render engine executes each Crossplane Function as a real
  container -- there is no mocked or in-memory function runtime. Without a
  running Docker daemon, rendering fails.
- **A Docker network for the render engine to join.** Create it once, before
  running any render:

  ```bash
  docker network create crossplane-render
  ```

  `crossplane-postrender` tells the render engine which network to join rather than
  letting it create a per-invocation one, so the network must already exist. The
  default name is `crossplane-render`; override it with `--docker-network` (see
  [Configuration](#configuration)).

  Note the network default is deliberately named after the *render engine*, not
  after this binary: the network is shared with the crossplane render engine and
  its function containers, and any containers left behind by a previous render are
  attached to it. Keeping the name stable is what lets those containers be reused.

  This also matters for reuse. Since crossplane CLI v2.4.0, an engine that sees a
  `render.crossplane.io/runtime-docker-network` annotation on a Function joins
  that network instead of creating one -- and consequently stops creating it. If
  you annotate your Functions that way (see
  [Reusing function containers](#reusing-function-containers)), point
  `--docker-network` at the same network so the engine and the reused containers
  can reach each other.
- **No `crossplane` CLI install required.** `crossplane-postrender` drives the
  crossplane CLI's render packages in-process (as a Go library dependency),
  rather than shelling out to a `crossplane` binary. There's nothing to
  install beyond the Go module itself and Docker.

## Quick start

Given a chart `example-chart` whose templates render an XRD, a Composition, a
Function package, and an `XBucket` XR with API group `platform.example.org`:

```bash
docker network create crossplane-render   # once, if it doesn't already exist

helm template example-chart ./example-chart \
  --post-renderer crossplane-postrender \
  --post-renderer-args platform.example.org
```

The API group domain (`platform.example.org` above) is a **required**
positional argument. It's how `crossplane-postrender` tells the XR it's supposed
to render apart from every other document in the stream -- a document whose
`apiVersion` contains that substring is the XR. Get it wrong and the tool
fails with an explicit "no XR found with API group domain" error rather than
silently rendering nothing.

Standard output normally shows the chart's XR, Composition, XRD, and Function
manifests. With the post-renderer in place, it instead shows whatever the
Composition's pipeline actually produced -- the composite resource (merged
back with the input XR's spec, equivalent to `crossplane render
--include-full-xr`) followed by every composed resource, sorted for stable
output.

### Helm v3 vs. Helm v4

The example above is Helm v3 syntax: `--post-renderer` takes a path to an
executable (resolved via `$PATH` if it contains no separator).

**Helm v4 changed this.** `--post-renderer` now takes the name of a
*registered plugin*, not a path -- pointing it directly at the
`crossplane-postrender` binary fails with `plugin: {Name:... Type:postrenderer/v1}
not found`. You must first register it as a `postrenderer/v1` plugin:

```bash
mkdir -p crossplane-postrender-plugin
cat > crossplane-postrender-plugin/plugin.yaml <<'EOF'
apiVersion: v1
type: postrenderer/v1
name: crossplane-postrender
version: 0.1.0
runtime: subprocess
runtimeConfig:
  platformCommand:
    - command: crossplane-postrender
EOF
helm plugin install crossplane-postrender-plugin/
```

Then invoke it by plugin name, exactly as in the Helm v3 example above:

```bash
helm template example-chart ./example-chart \
  --post-renderer crossplane-postrender \
  --post-renderer-args platform.example.org
```

One more Helm v4 detail worth calling out explicitly: `--post-renderer-args`
is how you pass the domain in *both* versions -- it is not the same as the
`--` separator some other CLIs use. Passing `-- platform.example.org` instead
does not work; Helm consumes the `--` itself and fails with "non-absolute
URLs should be in form of repo_name/path_to_chart", because it interprets
everything after `--` as chart-path arguments, not post-renderer arguments.

## Using it with helm-unittest

[helm-unittest][helm-unittest] supports a `postRenderer` block per test suite:

```yaml
suite: xbucket composition
templates:
  - templates/xbucket.yaml
  - templates/xbucket_comp.yaml
  - templates/xbucket_xrd.yaml
  - templates/functions.yaml
postRenderer:
  cmd: crossplane-postrender
  args:
    - platform.example.org
tests:
  - it: composes a Bucket with the right region
    set:
      region: us-west-2
    asserts:
      - isKind:
          of: Bucket
      - equal:
          path: spec.forProvider.region
          value: us-west-2
```

helm-unittest renders every listed template, then pipes the whole stream
through `crossplane-postrender` before assertions run -- so `templates:` must list
the XRD, Composition, and Function manifests alongside the XR, or the render
engine will be missing an input it needs (see [How it works](#how-it-works)
below). Assertions then run against the *composed* resources the pipeline
produced, not against the XR the chart declared.

## Configuration

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `<api-group-domain>` (positional, required) | -- | -- | Identifies which document in the stream is the XR to render: any document whose `apiVersion` contains this substring. |
| `--crossplane-version` | `CROSSPLANE_RENDER_VERSION` | `v2.4.0` | Pins the crossplane render-engine image. Left unset it would silently track "latest stable," which would let render behavior drift between runs without you asking for it. |
| `--docker-network` | `CROSSPLANE_DOCKER_NETWORK` | `crossplane-render` | The Docker network the render engine and its Function containers join. Must already exist (see [Prerequisites](#prerequisites)). |
| `-v`, `--verbose` | `CROSSPLANE_RENDER_VERBOSE` | `false` | Log render diagnostics to stderr. Never to stdout -- that stream is the manifest output Helm parses, and anything extra written there would corrupt it. |
| `-V`, `--version` | -- | -- | Print the tool's version and exit. |

Flags always take precedence over their corresponding environment variable.

## How it works

`crossplane-postrender` reads the entire input stream, classifies every document,
and feeds the classified inputs to the crossplane CLI's render engine:

| Document | How it's identified | Feeds into |
| --- | --- | --- |
| XR | `apiVersion` contains the configured API group domain | The composite resource to render |
| `Composition` | `kind: Composition` | The composition pipeline |
| `Function` | `kind: Function` | Function packages started as containers |
| `CompositeResourceDefinition` | `kind: CompositeResourceDefinition` | The XRD, matched to the XR by `spec.names.kind` or `spec.claimNames.kind` |
| `EnvironmentConfig` | `kind: EnvironmentConfig` | Required/extra resources |
| anything with `crossplane.io/test-observed: "true"` | annotation, checked before kind/apiVersion routing | Observed resources |
| anything with `crossplane.io/test-extra-resource: "true"` | annotation, checked before kind/apiVersion routing | Extra resources |
| anything else | -- | Dropped silently, not an error |

The two test-injection annotations exist so a test can supply synthetic
observed-resource state (to exercise a template branch that reads an existing
composed resource's status) or synthetic extra resources (so a
`function-go-templating` `ExtraResources` requirement resolves against test
data instead of failing). They're checked ahead of kind/apiVersion routing
specifically so an annotated XR-shaped document lands in the right bucket
instead of overwriting the XR actually being rendered.

Classification happens on parsed YAML documents, not on a text scan for the
first `kind:` line. That distinction matters: a Composition using
`function-go-templating` routinely embeds an inline Go template inside a
block scalar, and that block scalar can itself contain a literal `---` --
which a naive line-oriented splitter would treat as a document boundary and
use to corrupt the composition in half.

`crossplane-postrender` accepts the input stream in any of three shapes, since it
runs as either a `helm template` post-renderer or a helm-unittest
post-renderer, and the two produce different provenance markers:

1. **`#### file: <path>` markers** -- what helm-unittest injects ahead of
   each rendered document.
2. **`# Source: <path>` comments** -- what `helm template` emits.
3. **Helm v4's `postrenderer.helm.sh/postrender-filename` annotation**, with
   bare `---` separators and no comment marker at all. Helm v4 stamps this
   annotation on every document handed to a `postrenderer/v1` plugin.

Once classified, `crossplane-postrender` drives the crossplane CLI's render
engine in-process -- it imports
[`github.com/crossplane/cli/v2/cmd/crossplane/render`][cli-render] as a Go
library, rather than shelling out to a separately-installed `crossplane`
binary. `crossplane-diff` takes the same approach for the same reason: typed
Go values in, typed Go values out, no intermediate temp files to write and
re-parse.

[cli-render]: https://github.com/crossplane/cli/tree/main/cmd/crossplane/render

## Reusing function containers

Every render starts the Composition's Functions as containers, and starting them
is the dominant cost -- far more than the render itself. Since Helm spawns a
post-renderer process per unit test, a suite pays that cost once per test unless
the containers survive between invocations.

They can. Annotate each Function in your chart with a stable container name, an
`Orphan` cleanup policy, and the network `--docker-network` points at:

```yaml
apiVersion: pkg.crossplane.io/v1
kind: Function
metadata:
  name: function-go-templating
  annotations:
    render.crossplane.io/runtime-docker-name: function-go-templating-render
    render.crossplane.io/runtime-docker-cleanup: Orphan
    render.crossplane.io/runtime-docker-network: crossplane-render
spec:
  package: xpkg.crossplane.io/crossplane-contrib/function-go-templating:v0.12.2
```

`Orphan` leaves the container running when a render finishes, and the stable name
lets the next render find and reuse it rather than starting a fresh one. This is
the single largest lever on suite wall-clock time.

**The annotation and `--docker-network` must name the same network.** If they
disagree, the engine joins one network while the reusable containers sit on
another, and the failure is indirect enough to be hard to place:

```
cannot run Composition pipeline step "...": rpc error:
  code = DeadlineExceeded desc = ... produced zero addresses
```

That is the engine failing to resolve a Function it cannot reach — not a problem
with the Composition. A missing network is clearer (`network ... not found`), but
a *mismatched* one produces the message above. If you see it, check both names
agree before looking anywhere else.

Containers left running this way are yours to clean up. Note the filter matches
whatever suffix your `runtime-docker-name` annotations use, so adjust it —
and be deliberate, since removing them forfeits the reuse they exist for:

```bash
# Inspect first.
docker ps --filter 'name=-render' --format '{{.Names}}\t{{.Status}}\t{{.Networks}}'

# Then remove, if you actually want a cold start next run.
docker ps -aq --filter 'name=-render' | xargs -r docker rm -f
```

## Limitations / known issues

- **Docker is mandatory.** There is no mock-engine or Docker-less mode for the
  CLI entrypoint. (The test suite uses the crossplane CLI's `MockEngine` to
  test classification and orchestration without Docker, but that mock is not
  exposed to `crossplane-postrender`'s own users.)
- **Helm's contract limits how much invocation overhead can be removed.**
  Internally, the render engine supports batching many streams through one
  shared engine and function-runtime environment (`BatchRenderer.RenderAll`),
  which amortizes engine setup and container startup across every render
  instead of paying that cost per stream. But Helm's post-renderer contract is
  strictly one stream in, one process, one stream out -- there is no way for
  the `crossplane-postrender` binary itself to batch across the many separate
  `helm unittest` invocations a large test suite makes, because each
  invocation is a distinct process that knows nothing about the others.
  `BatchRenderer` exists for in-process Go callers that own their own render
  loop (a custom test harness, for instance) and are not bound by Helm's
  one-process-per-render contract; it is not something `helm template` or
  `helm unittest` can reach.
- **Helm v3 and v4 differ in how the post-renderer is invoked**, not in what
  `crossplane-postrender` does once invoked. See [Helm v3 vs. Helm
  v4](#helm-v3-vs-helm-v4) above.
- **Unroutable documents are dropped, not rejected.** A stray `ConfigMap` or
  any other manifest that doesn't match a known kind and isn't the XR is
  silently excluded from the render inputs. This is deliberate -- an unrelated
  manifest in the stream shouldn't fail a render -- but it means a
  misclassified document (for instance, an XR whose `apiVersion` doesn't
  actually contain the domain you passed) fails as "no XR found" rather than
  "found this input but ignored it."
- **Container reuse requires opting in per Function.** Reusing Function
  containers across renders (the difference between a test suite that takes
  minutes and one that takes an hour) requires annotating each `Function`
  manifest with a stable `render.crossplane.io/runtime-docker-name` and
  `render.crossplane.io/runtime-docker-cleanup: Orphan`. `crossplane-postrender`
  does not add these annotations for you; they belong on the Function
  manifests your chart templates.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, test/lint
commands, and code conventions.

## License

Apache License 2.0 -- see [LICENSE](LICENSE) for details.
