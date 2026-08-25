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
- **No network setup needed.** `crossplane-postrender` manages the Docker
  network itself: by default it lets the render engine create a throwaway
  network per invocation and remove it afterwards, so a one-shot render needs
  nothing pre-created. Pass `--reuse-containers` (see
  [Reusing function containers](#reusing-function-containers)) and it instead
  joins a persistent named network, creating it on demand if it doesn't
  already exist. Set `--docker-network` only if you want it to join a network
  you manage yourself (see [Configuration](#configuration)).
- **No `crossplane` CLI install required.** `crossplane-postrender` drives the
  crossplane CLI's render packages in-process (as a Go library dependency),
  rather than shelling out to a `crossplane` binary. There's nothing to
  install beyond the Go module itself and Docker.

## Quick start

Given a chart `example-chart` whose templates render an XRD, a Composition, a
Function package, and an `XBucket` XR with API group `platform.example.org`:

```bash
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
| `--docker-network` | `CROSSPLANE_DOCKER_NETWORK` | none; `crossplane-render` under `--reuse-containers` | The Docker network the render engine and its Function containers join. Left empty, the engine creates and removes a throwaway network per invocation. Under `--reuse-containers` it defaults to `crossplane-render` instead, created on demand if it doesn't exist, because reused containers must outlive the render that started them. Set this only to join a network you manage yourself. |
| `--reuse-containers` | `CROSSPLANE_REUSE_CONTAINERS` | `false` | Leave Function containers running and reuse them on the next render, instead of starting fresh ones every time. See [Reusing function containers](#reusing-function-containers). Off by default -- leaving containers running is a surprising side effect for a single render. |
| `--reuse-suffix` | `CROSSPLANE_REUSE_SUFFIX` | `render` | Distinguishes this project's reusable containers from another's. The default is shared on purpose (see [Reusing function containers](#reusing-function-containers)); pass your own suffix to opt out of sharing. Ignored unless `--reuse-containers` is set. |
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
the containers survive between invocations. Measured on the same input, warm
reused containers render in ~1.57s versus ~2.45s without reuse -- roughly 1.6x,
and it compounds across every test in a suite.

Turn it on with `--reuse-containers`, passed as an additional
`--post-renderer-args` alongside the required domain argument (Helm accepts
that flag more than once, appending each value to the post-renderer's
argument list):

```bash
helm template example-chart ./example-chart \
  --post-renderer crossplane-postrender \
  --post-renderer-args platform.example.org \
  --post-renderer-args --reuse-containers
```

Or set the environment variable once, so every invocation in a test run picks
it up without threading it through `--post-renderer-args` at all:

```bash
export CROSSPLANE_REUSE_CONTAINERS=true
```

**Your chart needs no annotations for this.** `crossplane-postrender` does the
following on your behalf, for every Function in the stream:

- Derives a stable container name from the Function's name and package
  version, in the form `<function-name>-<package-version>-<suffix>` -- for
  example `function-go-templating-v0.12.2-render`. The package version is
  included so upgrading a Function starts a fresh container instead of
  silently reusing one running the old image. A digest reference
  (`...@sha256:abc123`) keeps its algorithm, so two digests differing only in
  algorithm can't collide. The name is sanitized to what Docker accepts.
- Sets `Orphan` cleanup, so the container survives after the render finishes
  instead of being torn down.
- Creates the Docker network the reused containers run on
  (`crossplane-render` by default, or whatever `--docker-network` names) if it
  doesn't already exist -- there's nothing to pre-create yourself.

**A Function that already carries a `render.crossplane.io/runtime-docker-name`
or `render.crossplane.io/runtime-docker-cleanup` annotation is left completely
alone.** If your chart sets either one by hand, that explicit choice wins over
the derived one, all-or-nothing per Function.

Use `--reuse-suffix` (or `CROSSPLANE_REUSE_SUFFIX`) to change the suffix in the
derived name from its default, `render`. The default is shared on purpose:
Function containers are stateless gRPC servers keyed only by image, so two
projects reusing each other's containers is a feature -- whichever one starts
second gets an instant warm start instead of paying container startup again.
Pass your own suffix if you don't want that sharing.

Containers left running this way are yours to clean up. Filter on whatever
suffix you're using (`render` unless you set `--reuse-suffix`), inspect before
removing, and be deliberate -- removing them forfeits the reuse they exist for:

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
- **Container reuse is opt-in, and leaves containers running until you clean
  them up.** `--reuse-containers` is off by default specifically because
  leaving containers behind is a surprising side effect for a single render.
  Once enabled, containers persist across invocations until you remove them
  yourself -- see [Reusing function containers](#reusing-function-containers)
  for the cleanup snippet.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, test/lint
commands, and code conventions.

## License

Apache License 2.0 -- see [LICENSE](LICENSE) for details.
