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

// Package render turns a classified input stream into rendered manifests.
//
// It drives the crossplane CLI's render packages in-process rather than
// executing the `crossplane` binary. Two reasons:
//
//   - Correctness: inputs are passed as typed objects, so nothing depends on
//     writing temp files and re-parsing them.
//   - Speed: Helm spawns a post-renderer once per unit test, so process and
//     runtime setup dominate. In-process rendering removes CLI startup, and lets
//     a caller that owns the loop start function runtimes once and reuse them
//     across many renders (see BatchRenderer). That attacks invocation count,
//     which is where the cost actually is -- per-invocation micro-optimisation
//     cannot reach it.
//
// crossplane-contrib/crossplane-diff drives the same packages the same way; this
// follows its structure deliberately.
package render

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	"github.com/jcogilvie/crossplane-helm-postrender/internal/parse"
)

// DefaultCrossplaneVersion pins the core-crossplane render-engine image.
//
// Pinning matters: left unset, the engine resolves "latest stable", which drifts
// independently of the CLI version and can silently change render output between
// runs. Override with --crossplane-version to track a different release.
const DefaultCrossplaneVersion = "v2.4.0"

// DefaultDockerNetwork is the Docker network the render engine and function
// containers attach to.
//
// A stable, shared network is what makes container reuse possible. Annotate
// Functions with a stable render.crossplane.io/runtime-docker-name and
// runtime-docker-cleanup: Orphan, and their containers survive between renders
// for later invocations to reuse -- the difference between a test suite taking
// minutes and taking an hour.
//
// Since crossplane CLI v2.4.0, an engine that sees a runtime-docker-network
// annotation joins that network rather than creating its own, and consequently no
// longer creates it. The network must therefore already exist:
//
//	docker network create crossplane-render
const DefaultDockerNetwork = "crossplane-render"

// Options configures a Renderer.
type Options struct {
	// CrossplaneVersion pins the render-engine image. Defaults to
	// DefaultCrossplaneVersion when empty.
	CrossplaneVersion string

	// DockerNetwork is the network the render engine joins. Defaults to
	// DefaultDockerNetwork, so reused function containers stay reachable.
	DockerNetwork string

	// Logger receives render diagnostics. Defaults to a no-op logger.
	Logger logging.Logger
}

// Result is the outcome of one render.
//
// The composite is kept separate from the composed resources so callers can emit
// or assert on either. Both are included in the output stream, equivalent to
// `crossplane render --include-full-xr`.
type Result struct {
	Composite *unstructured.Unstructured
	Composed  []unstructured.Unstructured
}

// Manifests returns the rendered documents: the composite first, then composed
// resources.
func (r *Result) Manifests() []unstructured.Unstructured {
	out := make([]unstructured.Unstructured, 0, len(r.Composed)+1)
	if r.Composite != nil {
		out = append(out, *r.Composite)
	}
	out = append(out, r.Composed...)
	return out
}

// YAML marshals the result as a multi-document YAML stream.
func (r *Result) YAML() ([]byte, error) {
	var buf []byte
	for i, m := range r.Manifests() {
		b, err := yaml.Marshal(m.Object)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot marshal rendered manifest %d", i)
		}
		buf = append(buf, []byte("---\n")...)
		buf = append(buf, b...)
	}
	return buf, nil
}

// Renderer renders composite resources.
//
// It is an interface so callers can be tested without Docker: the crossplane CLI
// ships a MockEngine, and a fake Renderer keeps parse/emit tests hermetic.
type Renderer interface {
	// Render renders one classified stream.
	Render(ctx context.Context, s *parse.Stream, apiGroupDomain string) (*Result, error)
}

// BatchRenderer renders many streams while sharing one engine environment.
//
// This is the shape that makes the largest speedup available. Because Helm spawns
// a post-renderer process per unit test, per-invocation setup -- engine
// construction and function-runtime startup -- is paid once per test rather than
// once per run. A caller that owns the loop can pay it once for the whole batch.
//
// Renderer is deliberately kept separate: Helm's post-renderer contract is one
// stream in, one stream out, one process, so the CLI entrypoint can only ever use
// Renderer. BatchRenderer exists for in-process callers not bound by that
// contract -- a test harness that renders many charts, for instance.
type BatchRenderer interface {
	Renderer

	// RenderAll renders every stream against a shared engine environment.
	//
	// Results are returned in the same order as the inputs. A stream that fails
	// to render yields a nil result at its index and an error in the returned
	// slice, so one bad input does not discard the whole batch.
	RenderAll(ctx context.Context, streams []*parse.Stream, apiGroupDomain string) ([]*Result, []error)
}

// options normalises defaults so callers may pass a zero Options.
func (o Options) withDefaults() Options {
	if o.CrossplaneVersion == "" {
		o.CrossplaneVersion = DefaultCrossplaneVersion
	}
	if o.DockerNetwork == "" {
		o.DockerNetwork = DefaultDockerNetwork
	}
	if o.Logger == nil {
		o.Logger = logging.NewNopLogger()
	}
	return o
}
