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

	xr, err := compositeFrom(s.XR)
	if err != nil {
		return nil, err
	}

	comp, err := compositionFrom(s.Composition)
	if err != nil {
		return nil, err
	}

	fns, err := functionsFrom(s.Functions)
	if err != nil {
		return nil, err
	}

	observed, err := observedFrom(s.Observed)
	if err != nil {
		return nil, err
	}

	required, err := unstructuredFrom(s.EnvironmentConfigs)
	if err != nil {
		return nil, err
	}

	in := render.CompositionInputs{
		CompositeResource:   xr,
		Composition:         comp,
		FunctionCredentials: []corev1.Secret{},
		ObservedResources:   observed,
		RequiredResources:   required,
	}

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
	if xrdDoc := parse.MatchXRD(s.XR.Object, s.XRDs); xrdDoc != nil {
		xrd, err := xrdFrom(xrdDoc)
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

// compositeFrom converts the classified XR into the engine's composite type.
func compositeFrom(d *parse.Document) (*ucomposite.Unstructured, error) {
	xr := ucomposite.New()
	if err := yaml.Unmarshal(d.Raw, xr); err != nil {
		return nil, errors.Wrap(err, "cannot parse composite resource")
	}
	return xr, nil
}

// xrdFrom decodes an XRD into its typed form, which ApplyXRDDefaults requires
// in order to derive the CRD schema.
//
// Note this is the one place a typed XRD decode is unavoidable. Matching still
// uses unstructured accessors, because the apiserver round-trips XRDs through
// v1<->v2 conversion and the document's own apiVersion cannot be trusted.
func xrdFrom(d *parse.Document) (*apiextensionsv1.CompositeResourceDefinition, error) {
	xrd := &apiextensionsv1.CompositeResourceDefinition{}
	if err := yaml.Unmarshal(d.Raw, xrd); err != nil {
		return nil, errors.Wrapf(err, "cannot parse XRD from %s", sourceOf(d))
	}
	return xrd, nil
}

// compositionFrom decodes the Composition into its typed form, which the engine
// requires (unlike the other inputs, it is not passed as unstructured).
func compositionFrom(d *parse.Document) (*apiextensionsv1.Composition, error) {
	comp := &apiextensionsv1.Composition{}
	if err := yaml.Unmarshal(d.Raw, comp); err != nil {
		return nil, errors.Wrap(err, "cannot parse Composition")
	}
	return comp, nil
}

// functionsFrom decodes Function packages.
func functionsFrom(docs []parse.Document) ([]pkgv1.Function, error) {
	out := make([]pkgv1.Function, 0, len(docs))
	for i := range docs {
		fn := pkgv1.Function{}
		if err := yaml.Unmarshal(docs[i].Raw, &fn); err != nil {
			return nil, errors.Wrapf(err, "cannot parse Function from %s", sourceOf(&docs[i]))
		}
		out = append(out, fn)
	}
	return out, nil
}

// observedFrom decodes test-injected observed composed resources.
func observedFrom(docs []parse.Document) ([]composed.Unstructured, error) {
	out := make([]composed.Unstructured, 0, len(docs))
	for i := range docs {
		cd := composed.New()
		if err := yaml.Unmarshal(docs[i].Raw, cd); err != nil {
			return nil, errors.Wrapf(err, "cannot parse observed resource from %s", sourceOf(&docs[i]))
		}
		out = append(out, *cd)
	}
	return out, nil
}

// unstructuredFrom collects already-parsed documents for the required-resources
// input, reusing the objects the classifier produced rather than re-decoding.
func unstructuredFrom(docs []parse.Document) ([]unstructured.Unstructured, error) {
	out := make([]unstructured.Unstructured, 0, len(docs))
	for i := range docs {
		if docs[i].Object == nil {
			return nil, errors.Errorf("cannot use unparsed document from %s as a required resource", sourceOf(&docs[i]))
		}
		out = append(out, *docs[i].Object)
	}
	return out, nil
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
