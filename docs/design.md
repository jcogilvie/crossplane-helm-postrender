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

## Container reuse: why it lives in the tool, not in charts

Function-container startup is the dominant cost of a render -- far more than
the render itself -- and Helm spawns a post-renderer process once per unit
test, so a naive implementation pays that startup cost once per test in a
suite. `--reuse-containers` (`internal/render/reuse.go`) exists to remove
that, and its design has several decisions that are easy to "simplify" back
into a worse shape if you don't know why they're there.

### Why the annotations are injected by the tool, not written by hand in charts

The crossplane CLI already supports container reuse, through two
Function annotations (`render.crossplane.io/runtime-docker-name`,
`render.crossplane.io/runtime-docker-cleanup: Orphan`) that a chart author can
set by hand. The earliest version of this feature's documentation told users
to do exactly that.

That's the wrong owner for the decision. Container reuse is *this tool's*
answer to *its own* problem: Helm's one-process-per-render contract forces
`crossplane-postrender` to pay Function-startup cost on every invocation
unless something makes containers outlive a single render. That's an
implementation detail of how this post-renderer is invoked -- it has nothing
to do with what a chart's Composition does. Requiring a chart author to
hand-annotate every Function to opt into it, and to keep those annotations
correct as Functions are added, renamed, or upgraded, pushes a local
performance concern into the chart's own API. A chart that happens to get
tested by `crossplane-postrender` shouldn't need to know that fact reflected
in its templates, and a chart that stops being tested that way shouldn't need
its annotations cleaned up either.

`enableContainerReuse` derives the same annotations mechanically -- name from
the Function's own name and package version, cleanup fixed at `Orphan` -- and
applies them at render time, which means the decision to reuse containers is
made once, by the flag, at the place that actually has the problem to solve.
The one exception (`enableContainerReuse`'s early-continue when either
annotation is already present) exists so an explicit choice in a chart still
wins: reuse is opt-in by inference, not something that silently overrides an
author's own annotation. If you're tempted to make this override-in-force
instead, don't -- the point of leaving an explicit annotation alone is that
someone put it there for a reason (custom cleanup semantics, a fixed name
required by other tooling) that the derivation logic can't see.

### Why the network is created on demand instead of being a documented prerequisite

Without reuse, the render engine creates a throwaway Docker network per
invocation and tears it down afterward -- correct behavior for a one-shot
render, and it means a first-time user runs into zero Docker setup beyond
having the daemon running at all.

Reuse breaks that assumption: a container that's supposed to survive past the
render that started it needs a network that also survives past that render,
so the throwaway-per-invocation network can't be it. The earlier design
handled this by documenting `docker network create crossplane-render` as a
prerequisite step, on the theory that network lifecycle is the user's
business. In practice that just relocated the same "reuse is this tool's
problem" mistake from annotations to setup instructions -- and it produced a
worse failure mode than a normal missing-prerequisite error: skip the step
and the render fails deep inside container startup with `network ... not
found`, which reads like a Docker installation problem, not a missing `docker
network create` invocation three sections back in a README.

`ensureNetwork` (`internal/render/reuse.go`) removes the step instead of
documenting it better: it's called only when reuse is enabled, checks whether
the network already exists with `docker network inspect`, and creates it if
not, treating a concurrent "already exists" race as success rather than an
error (because two suites reusing containers in parallel, and therefore
racing to create the same network, is exactly the scenario reuse exists for).
Do not turn this back into a documented manual step; the entire point is that
a user enabling `--reuse-containers` needs to do nothing else.

### Why the tool does not set `runtime-docker-network`

`enableContainerReuse` sets the container-name and cleanup annotations but
deliberately leaves `render.crossplane.io/runtime-docker-network` untouched.
This looks like an oversight -- the other two reuse-relevant knobs are set,
why not this one? -- but setting it would create exactly the class of bug
this design otherwise avoids.

The crossplane CLI's own render engine already injects that annotation from
whichever network the engine itself joined for that render
(`CrossplaneDockerNetwork` in `engine.go`'s `Setup` call). If
`enableContainerReuse` also wrote a value for it, there would be two sources
of truth for the same fact -- the engine's actual joined network, and
whatever `enableContainerReuse` guessed independently -- with no mechanism
keeping them equal. A future change to `--docker-network`, or to how the
engine picks its network, could silently desync them.

That failure mode is not hypothetical; it is the exact bug this design
replaced. A mismatched (as opposed to missing) network annotation produces

```
cannot run Composition pipeline step "...": rpc error:
  code = DeadlineExceeded desc = ... produced zero addresses
```

which reads like a broken Composition step, not a networking mismatch,
because nothing about that error message points at Docker networking at all.
A *missing* network at least fails with a legible `network ... not found`.
Leaving the network annotation to the one place that actually knows the
answer -- the engine's own `Setup` -- removes the second source of truth
instead of trying to keep two sources in sync.

### Why off by default

`--reuse-containers` defaults to `false`. Reuse's whole mechanism is leaving
containers running after the process that started them exits -- which is
correct for a test suite invoking this post-renderer dozens of times, but a
surprising side effect for someone running one `helm template` command who
never asked for background containers left on their machine. Defaulting to
on would optimize for the repeated-invocation case at the cost of surprising
the single-invocation case, silently. Requiring an explicit flag means the
tradeoff (faster subsequent renders, in exchange for state left behind you
must clean up) is something a user opts into deliberately, not something they
discover later by finding containers they don't remember starting.

### Why the reuse suffix is shared by default

`DefaultReuseSuffix` is a fixed literal (`"render"`), not something derived
per-project (a working-directory hash, a repo name, anything unique). That's
deliberate, not a missed opportunity for isolation: a Function container is a
stateless gRPC server keyed entirely by its image -- it holds no
project-specific state between calls, so two unrelated projects reusing the
*same* container for the *same* Function image is strictly a win for
whichever one starts second, not a correctness risk. Making the default
project-specific would turn every second project on a machine into a cold
start, for no safety benefit, since there's nothing to isolate.

`--reuse-suffix` exists for the case where sharing is unwanted for other
reasons (e.g. wanting to force a distinct container per project regardless of
image identity) -- but that's an explicit opt-out a user reaches for, not the
default anyone should need to protect themselves from.

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
