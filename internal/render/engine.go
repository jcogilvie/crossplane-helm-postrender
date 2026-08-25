/*
Copyright 2026 Jonathan Ogilvie.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package render

import (
	"context"
	"slices"
	"strings"

	"dario.cat/mergo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"
	ucomposite "github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"
	"github.com/crossplane/crossplane-runtime/v2/pkg/xcrd"

	apiextensionsv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"

	"github.com/crossplane/cli/v2/cmd/crossplane/render"
	xrpkg "github.com/crossplane/cli/v2/pkg/xr"

	"github.com/jcogilvie/crossplane-helm-postrender/internal/parse"
)

// engineRenderer renders via the crossplane CLI's render engine, in-process.
type engineRenderer struct {
	o Options

	// newEngine builds the render engine. It is a field so tests can inject
	// render.MockEngine and run without Docker; production callers get the real
	// Docker-backed engine from NewRenderer.
	newEngine func() render.Engine

	// startRuntimes starts function runtimes. Also injectable, because starting
	// real runtimes requires Docker.
	startRuntimes func(ctx context.Context, fns []pkgv1.Function) (*render.FunctionAddresses, error)
}

// NewRenderer returns a Renderer backed by the crossplane render engine.
func NewRenderer(o Options) Renderer {
	opts := o.withDefaults()
	return &engineRenderer{
		o: opts,
		newEngine: func() render.Engine {
			// The engine must join the shared network rather than create its
			// own, so that reused function containers remain reachable.
			return render.NewEngineFromFlags(&render.EngineFlags{
				CrossplaneVersion:       opts.CrossplaneVersion,
				CrossplaneDockerNetwork: opts.DockerNetwork,
			}, opts.Logger)
		},
		startRuntimes: func(ctx context.Context, fns []pkgv1.Function) (*render.FunctionAddresses, error) {
			return render.StartFunctionRuntimes(ctx, opts.Logger, fns)
		},
	}
}

// Render renders one classified stream, setting up and tearing down its own
// engine environment.
//
// This is the path Helm's post-renderer contract requires -- one stream in, one
// stream out, one process. Callers rendering more than one stream should use
// RenderAll, which pays engine and runtime setup once for the whole batch.
func (e *engineRenderer) Render(ctx context.Context, s *parse.Stream, apiGroupDomain string) (*Result, error) {
	results, errs := e.RenderAll(ctx, []*parse.Stream{s}, apiGroupDomain)
	if errs[0] != nil {
		return nil, errs[0]
	}
	return results[0], nil
}

// RenderAll renders every stream against one shared engine environment.
//
// The engine's Setup is documented as safe to call more than once to integrate
// additional functions into an environment a prior call created, which is what
// makes sharing sound: each stream declares its own Functions, and a stream
// whose functions are already running reuses them.
func (e *engineRenderer) RenderAll(ctx context.Context, streams []*parse.Stream, apiGroupDomain string) ([]*Result, []error) {
	results := make([]*Result, len(streams))
	errs := make([]error, len(streams))

	// Per-stream inputs are converted up front so a malformed stream fails
	// without disturbing the shared environment other streams depend on.
	prepared := make([]*compositionInput, len(streams))
	var allFns []pkgv1.Function
	for i, s := range streams {
		in, err := e.prepare(s, apiGroupDomain)
		if err != nil {
			errs[i] = err
			continue
		}
		prepared[i] = in
		allFns = append(allFns, in.functions...)
	}

	// Nothing renderable: return the per-stream errors as they are.
	if !slices.ContainsFunc(prepared, func(p *compositionInput) bool { return p != nil }) {
		return results, errs
	}

	eng := e.newEngine()

	cleanup, err := eng.Setup(ctx, dedupeFunctions(allFns))
	if err != nil {
		return results, fillRemaining(errs, prepared, errors.Wrap(err, "cannot set up render engine"))
	}
	if cleanup != nil {
		defer cleanup()
	}

	addrs, err := e.startRuntimes(ctx, dedupeFunctions(allFns))
	if err != nil {
		return results, fillRemaining(errs, prepared, errors.Wrap(err, "cannot start function runtimes"))
	}
	defer render.StopFunctionRuntimes(e.o.Logger, addrs)

	// FunctionAddresses.Addresses panics on a nil receiver, unlike
	// StopFunctionRuntimes which tolerates one. Guard rather than assume the
	// starter always returns a value.
	fnAddrs := map[string]string{}
	if addrs != nil {
		fnAddrs = addrs.Addresses()
	}

	for i, in := range prepared {
		if in == nil {
			continue
		}
		res, err := e.renderOne(ctx, eng, in, fnAddrs)
		if err != nil {
			errs[i] = err
			continue
		}
		results[i] = res
	}

	return results, errs
}

// fillRemaining assigns a shared-setup failure to every stream that had prepared
// successfully, since none of them can now be rendered.
func fillRemaining(errs []error, prepared []*compositionInput, err error) []error {
	for i := range prepared {
		if prepared[i] != nil {
			errs[i] = err
		}
	}
	return errs
}

// dedupeFunctions collapses Functions declared by more than one stream, keyed by
// name. Passing the same Function twice would start redundant runtimes.
func dedupeFunctions(fns []pkgv1.Function) []pkgv1.Function {
	seen := make(map[string]struct{}, len(fns))
	out := make([]pkgv1.Function, 0, len(fns))
	// Indexed rather than ranged by value: pkgv1.Function is a large struct and
	// this runs on the render path.
	for i := range fns {
		name := fns[i].GetName()
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, fns[i])
	}
	return out
}

// compositionInput is one stream's converted, ready-to-render inputs.
type compositionInput struct {
	xr        *ucomposite.Unstructured
	inputs    render.CompositionInputs
	functions []pkgv1.Function
}

// prepare converts and validates one stream without touching the engine.
func (e *engineRenderer) prepare(s *parse.Stream, apiGroupDomain string) (*compositionInput, error) {
	if err := s.Validate(apiGroupDomain); err != nil {
		return nil, err
	}

	// The decodes are written as expressions against a shared decoder, which
	// accumulates the first error and makes later calls no-ops, so this reads as a
	// description of the render inputs instead of five repetitions of
	// `x, err := ...; if err != nil { return nil, err }`. Checked once below.
	//
	// Sound only because these decodes are independent: none consumes a value
	// another produces. The XRD step further down is deliberately left out for
	// exactly that reason -- it needs the decoded XR, and it mutates it.
	var d decoder

	fns := decodeEach(&d, s.Functions, "Function", zeroOf[pkgv1.Function])
	in := render.CompositionInputs{
		CompositeResource:   decodeOne(&d, s.XR, "composite resource", newComposite),
		Composition:         decodeOne(&d, s.Composition, "Composition", zeroOf[apiextensionsv1.Composition]),
		FunctionCredentials: []corev1.Secret{},
		ObservedResources:   decodeEach(&d, s.Observed, "observed resource", newComposed),
		// Reuses the objects the classifier already parsed rather than decoding
		// them again.
		RequiredResources: objectsOf(s.EnvironmentConfigs),
	}

	if d.err != nil {
		return nil, d.err
	}

	xr := in.CompositeResource

	// The XRD does two distinct jobs, and both are required.
	//
	// Passing it as an input selects the composite schema (Legacy vs Modern) for
	// the XR's GVK, mirroring the production reconciler. But that alone does NOT
	// populate the XR's schema defaults -- ApplyXRDDefaults must be called on
	// the XR before rendering. Without it, composition templates that read
	// XRD-defaulted fields (.spec.mode, .spec.providerConfig, and friends)
	// resolve to "<no value>" and render structurally valid but wrong output.
	//
	// Omitting the XRD entirely when none matches is correct and matches
	// `crossplane render` invoked without --xrd: the engine falls back to Modern.
	//
	// Typed decoding is unavoidable here, since ApplyXRDDefaults needs it to derive
	// the CRD schema -- even though *matching* uses unstructured accessors, because
	// the apiserver round-trips XRDs through v1<->v2 conversion and a document's
	// own apiVersion cannot be trusted.
	if xrdDoc := parse.MatchXRD(s.XR.Object, s.XRDs); xrdDoc != nil {
		xrd, err := decodeDocument(xrdDoc, "XRD", zeroOf[apiextensionsv1.CompositeResourceDefinition])
		if err != nil {
			return nil, err
		}
		if err := xrpkg.ApplyXRDDefaults(xr.GetUnstructured(), xrd); err != nil {
			return nil, errors.Wrapf(err, "cannot apply XRD defaults to XR %q", xr.GetName())
		}
		in.XRD = xrdDoc.Object
	}

	return &compositionInput{xr: xr, inputs: in, functions: fns}, nil
}

// renderOne renders a single prepared input against an already-configured
// engine, using function addresses the caller established.
func (e *engineRenderer) renderOne(ctx context.Context, eng render.Engine, p *compositionInput, fnAddrs map[string]string) (*Result, error) {
	in := p.inputs
	in.FunctionAddrs = fnAddrs
	xr := p.xr

	req, err := render.BuildCompositeRequest(in)
	if err != nil {
		return nil, errors.Wrap(err, "cannot build render request")
	}

	rsp, err := eng.Render(ctx, req)
	if err != nil {
		return nil, errors.Wrap(err, "cannot render composite resource")
	}

	out, err := render.ParseCompositeResponse(rsp.GetComposite())
	if err != nil {
		return nil, errors.Wrap(err, "cannot parse render response")
	}

	res := &Result{Composed: make([]unstructured.Unstructured, 0, len(out.ComposedResources))}

	// The engine returns only the *desired* composite, which carries no spec, so
	// the input XR's spec and metadata are merged back in. This mirrors the CLI's
	// own implementation of --include-full-xr, including its caveat: the merge
	// cannot be perfectly accurate, because we do not know how the apiserver
	// would merge lists.
	if out.CompositeResource != nil {
		merged := xr.DeepCopy()
		if err := mergo.Merge(&merged.Object, out.CompositeResource.Object, mergo.WithOverride); err != nil {
			return nil, errors.Wrap(err, "cannot merge rendered composite into the input XR")
		}
		// Condition timestamps come from the engine as real wall-clock times,
		// which would make output differ on every run. Replace them with the
		// stable value the CLI uses so results are comparable and assertions on
		// them are meaningful.
		if err := render.ReplaceConditionTimestamps(&merged.Unstructured); err != nil {
			return nil, errors.Wrap(err, "cannot replace condition timestamps in composite resource")
		}
		res.Composite = &unstructured.Unstructured{Object: merged.Object}
	}

	for i := range out.ComposedResources {
		if err := render.ReplaceConditionTimestamps(&out.ComposedResources[i].Unstructured); err != nil {
			return nil, errors.Wrapf(err, "cannot replace condition timestamps in composed resource %q",
				out.ComposedResources[i].GetName())
		}
		res.Composed = append(res.Composed, unstructured.Unstructured{
			Object: out.ComposedResources[i].Object,
		})
	}

	// Sort by composition-resource-name so output is stable across runs; the
	// engine does not guarantee an order. Tests select documents rather than
	// index them, but unstable ordering still makes diffs unreadable.
	slices.SortStableFunc(res.Composed, func(a, b unstructured.Unstructured) int {
		return strings.Compare(
			a.GetAnnotations()[xcrd.AnnotationKeyCompositionResourceName],
			b.GetAnnotations()[xcrd.AnnotationKeyCompositionResourceName],
		)
	})

	return res, nil
}

// decodeDocument unmarshals one document into a new T.
//
// The crossplane CLI ships equivalents of these decoders -- render.LoadFunctions,
// LoadComposition, LoadXRD, LoadCompositeResource and friends -- but every one
// takes (afero.Fs, path) and reads from a file. We hold per-document bytes
// already split out of a single stdin stream, so using them would mean writing
// those bytes back out to a filesystem purely to have the library read and parse
// them again: the temp-file round-trip that rendering in-process exists to avoid.
// These helpers are the bytes-shaped counterpart of a path-shaped API, not a
// reimplementation of it.
//
// `newT` supplies the zero value because some target types need a constructor
// rather than a zero struct: composite.New() and composed.New() initialise the
// embedded Object map, and unmarshalling into an uninitialised one fails.
//
// `what` names the kind in error messages. Errors always name the source
// document, since a parse failure on one document in a stream is otherwise very
// hard to place.
func decodeDocument[T any](d *parse.Document, what string, newT func() *T) (*T, error) {
	out := newT()
	if err := yaml.Unmarshal(d.Raw, out); err != nil {
		return nil, errors.Wrapf(err, "cannot parse %s from %s", what, sourceOf(d))
	}
	return out, nil
}

// decodeDocuments decodes every document in a slice, dereferencing the results.
func decodeDocuments[T any](docs []parse.Document, what string, newT func() *T) ([]T, error) {
	out := make([]T, 0, len(docs))
	for i := range docs {
		v, err := decodeDocument(&docs[i], what, newT)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, nil
}

// Constructors for the decode target types.
//
// zeroOf covers plain structs. composite.New and composed.New cannot be passed
// directly -- they are `func(opts ...Option) *T`, which does not satisfy
// `func() *T` -- so they are wrapped. They are needed at all because they
// initialise the embedded Object map, and unmarshalling into an uninitialised one
// fails.
func zeroOf[T any]() *T                      { return new(T) }
func newComposite() *ucomposite.Unstructured { return ucomposite.New() }
func newComposed() *composed.Unstructured    { return composed.New() }

// decoder accumulates the first decode error so a group of independent decodes
// can be written as expressions and checked once, rather than each one being
// interrupted by its own `if err != nil`.
//
// It deliberately keeps the *first* error rather than the last: later decodes are
// no-ops once one has failed, so a subsequent error would be less informative,
// not more. Only sound for decodes that do not depend on each other's results.
type decoder struct {
	err error
}

// decodeOne decodes a single document, or returns nil if the decoder has already
// failed.
func decodeOne[T any](d *decoder, doc *parse.Document, what string, newT func() *T) *T {
	if d.err != nil {
		return nil
	}
	out, err := decodeDocument(doc, what, newT)
	if err != nil {
		d.err = err
		return nil
	}
	return out
}

// decodeEach decodes every document in a slice, or returns nil if the decoder has
// already failed.
//
// Note for Functions specifically: a v1beta1 Function decodes into the v1 type
// without loss, since both FunctionSpecs are structurally identical, embedding
// only PackageSpec and PackageRuntimeSpec. The CLI's own LoadFunctions accepts
// both apiVersions for the same reason.
func decodeEach[T any](d *decoder, docs []parse.Document, what string, newT func() *T) []T {
	if d.err != nil {
		return nil
	}
	out, err := decodeDocuments(docs, what, newT)
	if err != nil {
		d.err = err
		return nil
	}
	return out
}

// objectsOf collects the objects the classifier already parsed, without decoding
// again. Every document the classifier routed to a bucket other than Unknown has
// a non-nil Object by construction, so there is nothing to fail here.
func objectsOf(docs []parse.Document) []unstructured.Unstructured {
	out := make([]unstructured.Unstructured, 0, len(docs))
	for i := range docs {
		out = append(out, *docs[i].Object)
	}
	return out
}

// sourceOf names a document for error messages, preferring its template path and
// falling back to kind/name so the message is never empty.
func sourceOf(d *parse.Document) string {
	if d.Source != "" {
		return d.Source
	}
	if d.Object != nil {
		if n := d.Object.GetName(); n != "" {
			return d.Object.GetKind() + "/" + n
		}
		return d.Object.GetKind()
	}
	return "unknown source"
}
