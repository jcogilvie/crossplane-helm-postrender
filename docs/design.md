# Design

This document describes how `crossplane-postrender` is put together, why it's
structured the way it is, and three behaviors that are easy to get wrong and
were each found by diffing output against `crossplane render` -- not by
reading documentation. If you're changing `internal/render` or
`internal/parse`, read the "Behaviors worth preserving" section before you
start; it exists specifically so a future refactor doesn't quietly undo one
of them.

## Package layout

```
cmd/crossplane-postrender/   CLI entrypoint (kong-based argument parsing, stdin/stdout wiring)
internal/parse/          Classifies an input YAML stream into typed render inputs
internal/render/         Drives the crossplane CLI's render engine against classified inputs
internal/version/        Reports the build version
```

### `cmd/crossplane-postrender`

`main.go` owns exactly two responsibilities: parsing CLI flags (via
[`kong`](https://github.com/alecthomas/kong)) and wiring `os.Stdin`/
`os.Stdout` to the `cli.Run()` method that does the actual work. `Run()`
takes `Stdin`/`Stdout` as struct fields rather than reading the globals
directly, specifically so tests can inject `strings.Reader`s and buffers
without touching process-wide state (`cmd/crossplane-postrender/main_test.go`
exercises this directly).

The one config decision that lives here rather than in `internal/render`:
verbose logging goes to stderr, never stdout, because stdout is the parsed
manifest stream Helm is going to read back in. A single stray log line
written to stdout would corrupt that stream in a way that's genuinely
confusing to debug from the Helm side (Helm just reports "plugin exited with
error" or, worse, fails YAML-parsing the mangled output).

### `internal/parse`

Turns a raw byte stream into a `Stream` struct: one `XR`, one `Composition`,
and slices of `Function`, `XRD`, `EnvironmentConfig`, and test-injected
`Observed`/`ExtraResource` documents, plus an `Unknown` bucket for anything
that didn't match. `parse.go` does document splitting and classification;
`xrd.go` does the one piece of XRD-specific logic that doesn't belong in the
general classifier -- matching an XR's `kind` against a candidate XRD's
`spec.names.kind`/`spec.claimNames.kind`.

### `internal/render`

`render.go` defines the `Renderer`/`BatchRenderer` interfaces and the
`Result` type; `engine.go` is the one implementation, `engineRenderer`,
which drives `github.com/crossplane/cli/v2/cmd/crossplane/render` in-process.
`Renderer` and `BatchRenderer` are split into two interfaces on purpose (see
[Batching](#batching-and-why-helm-cant-use-it) below) even though
`engineRenderer` implements both.

## Data flow

```
stdin (YAML stream)
  │
  ▼
parse.Parse()                    -- split documents, classify each one
  │
  ▼
parse.Stream                     -- XR, Composition, []Function, []XRD, ...
  │
  ▼
parse.MatchXRD()                 -- find the XRD (if any) that names the XR's kind
  │
  ▼
engineRenderer.prepare()         -- convert to typed inputs, apply XRD defaults
  │
  ▼
render.Engine.Setup() + Render()  -- start Function containers, run the pipeline
  │
  ▼
engineRenderer.renderOne()       -- merge composite back into input XR, normalize
  │
  ▼
Result.YAML()                    -- marshal composite + composed resources
  │
  ▼
stdout (YAML stream)
```

Validation (`Stream.Validate`) runs at the boundary between parsing and
rendering, before any engine or Docker work starts. This ordering is
deliberate: a malformed stream should fail with "no XR found with API group
domain %q" or similar, immediately, rather than after paying the cost of
starting a Docker network connection and Function containers that were never
going to be used.

## Why in-process rather than shelling out

`crossplane-postrender` imports the crossplane CLI's render packages as a Go
library dependency and calls them directly, rather than exec'ing a separately
installed `crossplane` binary and parsing its stdout/stderr.
[`crossplane-diff`](https://github.com/crossplane-contrib/crossplane-diff)
takes the same approach for the same two reasons:

1. **Correctness.** Render inputs are passed as typed Go values
   (`render.CompositionInputs`, populated straight from the parsed
   `unstructured.Unstructured` objects) rather than serialized to temp files
   and re-parsed by a subprocess. There's no intermediate text
   representation to get subtly wrong, and no temp-file lifecycle to manage.
2. **Speed, structurally.** Helm invokes a post-renderer once per render --
   and `helm unittest` invokes the post-renderer process once per *test
   case* in a suite that specifies one. Per-invocation overhead (process
   startup for `crossplane-postrender` itself, but especially engine and
   Function-container setup) dominates the actual render cost for a
   realistic composition. In-process rendering doesn't add CLI-subprocess
   startup on top of that, and -- more importantly -- it makes engine and
   runtime setup available to reuse programmatically (see below), which
   removing a CLI's own process-launch overhead never could.

## Batching and why Helm can't use it

`engineRenderer.RenderAll` renders many `parse.Stream`s against **one**
shared `render.Engine` and one shared set of running Function containers,
rather than tearing an engine down and building a new one per stream. The
crossplane CLI documents `Engine.Setup` as safe to call more than once to
integrate additional Functions into an environment a prior call already
created -- that's the property that makes sharing sound in the first place.
`RenderAll` exploits it: every stream in the batch declares its own
`Functions`, duplicates across streams are deduplicated by name
(`dedupeFunctions`), and a stream whose Functions are already running simply
reuses them instead of starting a second copy.

`engineRenderer.Render` -- the single-stream method the CLI entrypoint
actually calls -- is implemented as `RenderAll` called with a
one-stream slice. It exists as a thin wrapper specifically so the two paths
cannot drift apart; there is no separate single-stream code path to keep in
sync.

**The structural limitation, stated plainly: `BatchRenderer.RenderAll` is not
reachable from `helm template` or `helm unittest`.** Helm's post-renderer
contract is one process, one input stream, one output stream -- Helm starts
a fresh `crossplane-postrender` process for every render it wants post-processed,
and each process has no way to know about, or share state with, any other
invocation. There is no flag or protocol by which Helm could hand
`crossplane-postrender` "here are ten streams, batch them" -- the contract simply
doesn't have a slot for that. `BatchRenderer` therefore exists purely for
in-process Go callers that own their own render loop and are not bound by
Helm's contract: a custom test harness that wants to render many chart
variants in one process, for example. If you're tempted to expose
`RenderAll` through the `crossplane-postrender` binary's CLI to "fix" this
limitation, the fix has to happen on Helm's side of the contract, not this
tool's -- there's no stdin/stdout protocol that lets one post-renderer
process render more than the one stream Helm handed it.

The performance delta this unlocks is provable without Docker:
`internal/render/batch_bench_test.go` benchmarks `Render` (per-stream setup)
against `RenderAll` (setup once) using a `MockEngine` whose `Setup` and
function-runtime start each sleep for a fixed duration standing in for real
Docker/container overhead -- real measured per-invocation overhead is on the
order of a second against Docker; the benchmark uses a smaller constant to
keep it fast while preserving the ratio. The benchmark exists to keep this
property honest under future changes, not to make a specific numeric claim
about production speedup -- the actual multiplier depends on how many
streams a caller batches and how expensive their specific Functions are to
start.

## Behaviors worth preserving

Each of these was discovered by diffing this tool's output against
`crossplane render`'s own output for the same inputs, not by reading a
doc that said so up front. All three are easy to accidentally regress
because the code compiles and most tests still pass without them --
only the specific behavior they exist for breaks.

### 1. `ApplyXRDDefaults` must run on the XR, separately from passing the XRD as a render input

Passing an XRD into `render.CompositionInputs.XRD` selects which composite
schema variant (Legacy v. Modern) the engine treats the XR's GVK as having --
mirroring what the production Crossplane reconciler does. **It does not, by
itself, populate the XR's schema-declared default values.** If a
Composition's template reads a field the XRD defaults (`.spec.mode`,
`.spec.providerConfigRef`, anything with an OpenAPI `default:`), and the
input XR document doesn't set that field explicitly, the rendered value is
`<no value>` -- structurally valid YAML, silently wrong content, and no error
anywhere to point at the cause.

The fix, in `internal/render/engine.go`'s `prepare`, is to call
`xrpkg.ApplyXRDDefaults(xr.GetUnstructured(), xrd)` on the XR *before*
building the render request, whenever `parse.MatchXRD` found a matching XRD.
Note the ordering dependency this creates: `MatchXRD` has to run first
(it needs the XR's `kind` and the XRD list), then `ApplyXRDDefaults` mutates
the XR object in place, and only then does `render.BuildCompositeRequest`
read the (now-defaulted) XR. Reordering these -- for instance, building the
request before applying defaults -- reintroduces the `<no value>` bug
silently, because nothing else in the render path checks that defaulting
happened.

When no XRD matches (`parse.MatchXRD` returns `nil`), the XRD is omitted from
the render input entirely. This matches `crossplane render` invoked without
`--xrd`: the engine falls back to treating the composite as Modern. That's
correct, not a fallback to work around -- don't add a "no XRD found" error
here.

### 2. The engine's response composite has no spec; the input XR's spec/metadata must be merged back in

`render.Engine.Render` returns a *desired* composite resource, but that
desired composite is deliberately minimal -- it doesn't carry the input XR's
own spec or most of its metadata, because from the engine's point of view
those are inputs, not outputs of the pipeline. If `renderOne` returned that
response composite as-is, the rendered composite output would be missing
almost everything the user actually put in their XR.

The fix (`internal/render/engine.go`'s `renderOne`) deep-copies the *input*
XR and merges the engine's response into it with
`mergo.Merge(&merged.Object, out.CompositeResource.Object, mergo.WithOverride)`
-- input fields survive, and any field the pipeline actually set overrides
the input value. This is deliberately the same operation the crossplane CLI
itself performs for `--include-full-xr`, including its documented caveat:
the merge is not guaranteed accurate for lists, because neither this tool nor
the CLI knows how the Kubernetes apiserver's own merge semantics would
reconcile a list field the pipeline touched against the same list field in
the input. If you're debugging a case where a list field in the rendered
composite doesn't look right, this is very likely why -- it is a known
limitation inherited from upstream, not a new bug in this repo.

### 3. Condition timestamps and composed-resource order must be normalized before output

Two independent instabilities, both fixed in `renderOne`, both there so
output is comparable across runs and diffable in a test assertion:

- **Timestamps.** Conditions the engine attaches (`lastTransitionTime`, and
  similar) carry real wall-clock time. Left alone, every render produces
  different output even for byte-identical inputs, which makes snapshot
  testing (`helm unittest -u` / `matchSnapshot`) useless -- the snapshot
  would need updating on every run regardless of whether anything
  meaningful changed. `render.ReplaceConditionTimestamps` is called on both
  the merged composite and every composed resource to replace real
  timestamps with the stable placeholder value the crossplane CLI itself
  uses for the same reason.
- **Ordering.** The engine does not guarantee the order composed resources
  come back in. `renderOne` sorts `res.Composed` by each resource's
  `crossplane.io/composition-resource-name` annotation
  (`xcrd.AnnotationKeyCompositionResourceName`) using
  `slices.SortStableFunc`, specifically so two renders of the same inputs
  produce byte-identical output. Tests that select a resource by name or
  kind aren't affected either way, but unstable ordering still makes a raw
  diff of rendered output (for debugging, or in a snapshot test that dumps
  the whole stream) unreadable -- the "diff" would be dominated by resources
  changing position rather than content.

If you're adding a new field to `Result` or a new post-processing step in
`renderOne`, put it after these two normalization steps, not before --
otherwise whatever you add will see un-normalized timestamps or ordering and
inherit the same non-determinism these exist to remove.
