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
	"strings"
	"testing"

	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"

	"github.com/crossplane/cli/v2/cmd/crossplane/render"
	renderv1alpha1 "github.com/crossplane/cli/v2/proto/render/v1alpha1"

	"github.com/jcogilvie/crossplane-helm-postrender/internal/parse"
)

// The point of RenderAll is that engine and runtime setup are paid once for the
// whole batch rather than once per stream. If this regresses, the batch path has
// no reason to exist.
func TestRenderAllSetsUpOnce(t *testing.T) {
	setups, starts := 0, 0

	eng := &render.MockEngine{
		MockSetup: func(_ context.Context, _ []pkgv1.Function) (func(), error) {
			setups++
			return func() {}, nil
		},
	}

	r := &engineRenderer{
		o:         Options{}.withDefaults(),
		newEngine: func() render.Engine { return eng },
		startRuntimes: func(_ context.Context, _ []pkgv1.Function) (*render.FunctionAddresses, error) {
			starts++
			return nil, nil
		},
	}

	const n = 5
	streams := make([]*parse.Stream, n)
	for i := range streams {
		streams[i] = parseStream(t, completeStream)
	}

	results, errs := r.RenderAll(context.Background(), streams, testDomain)

	for i := range errs {
		if errs[i] != nil {
			t.Errorf("RenderAll(...): stream %d: unexpected error: %v", i, errs[i])
		}
		if results[i] == nil {
			t.Errorf("RenderAll(...): stream %d: want a result, got nil", i)
		}
	}
	if setups != 1 {
		t.Errorf("RenderAll(...) over %d streams: engine Setup called %d times, want 1", n, setups)
	}
	if starts != 1 {
		t.Errorf("RenderAll(...) over %d streams: runtimes started %d times, want 1", n, starts)
	}
}

// One malformed stream must not discard the rest of the batch, or a single bad
// test case would fail every other one sharing the run.
func TestRenderAllIsolatesFailures(t *testing.T) {
	const noXR = `apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: c
---
apiVersion: pkg.crossplane.io/v1
kind: Function
metadata:
  name: f
`

	streams := []*parse.Stream{
		parseStream(t, completeStream),
		parseStream(t, noXR), // invalid: no XR
		parseStream(t, completeStream),
	}

	r := testRenderer(t, &render.MockEngine{})
	results, errs := r.RenderAll(context.Background(), streams, testDomain)

	if errs[0] != nil {
		t.Errorf("RenderAll(...): stream 0: unexpected error: %v", errs[0])
	}
	if results[0] == nil {
		t.Error("RenderAll(...): stream 0: want a result, got nil")
	}

	if errs[1] == nil {
		t.Error("RenderAll(...): stream 1: want an error for the invalid stream, got nil")
	} else if !strings.Contains(errs[1].Error(), "no XR found") {
		t.Errorf("RenderAll(...): stream 1: want a no-XR error, got %q", errs[1])
	}
	if results[1] != nil {
		t.Error("RenderAll(...): stream 1: want no result for the invalid stream")
	}

	// The valid stream after the invalid one must still render.
	if errs[2] != nil {
		t.Errorf("RenderAll(...): stream 2: unexpected error: %v", errs[2])
	}
	if results[2] == nil {
		t.Error("RenderAll(...): stream 2: want a result, got nil")
	}
}

// A shared-setup failure applies to every stream that would otherwise have
// rendered -- none of them can proceed without an engine.
func TestRenderAllReportsSharedSetupFailure(t *testing.T) {
	eng := &render.MockEngine{
		MockSetup: func(_ context.Context, _ []pkgv1.Function) (func(), error) {
			return nil, errBoom
		},
	}

	streams := []*parse.Stream{
		parseStream(t, completeStream),
		parseStream(t, completeStream),
	}

	_, errs := testRenderer(t, eng).RenderAll(context.Background(), streams, testDomain)
	for i := range errs {
		if errs[i] == nil {
			t.Errorf("RenderAll(...): stream %d: want the setup error, got nil", i)
		}
	}
}

// When no stream is renderable the engine must not be touched at all.
func TestRenderAllSkipsEngineWhenNothingValid(t *testing.T) {
	eng := &render.MockEngine{
		MockSetup: func(_ context.Context, _ []pkgv1.Function) (func(), error) {
			t.Error("RenderAll(...): engine Setup called with no valid streams")
			return func() {}, nil
		},
	}

	streams := []*parse.Stream{parseStream(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n")}

	results, errs := testRenderer(t, eng).RenderAll(context.Background(), streams, testDomain)
	if errs[0] == nil {
		t.Error("RenderAll(...): want an error for the unrenderable stream, got nil")
	}
	if results[0] != nil {
		t.Error("RenderAll(...): want no result for the unrenderable stream")
	}
}

// Functions shared across streams must be started once, not once per stream.
func TestRenderAllDedupesFunctions(t *testing.T) {
	var got []pkgv1.Function

	r := &engineRenderer{
		o:         Options{}.withDefaults(),
		newEngine: func() render.Engine { return &render.MockEngine{} },
		startRuntimes: func(_ context.Context, fns []pkgv1.Function) (*render.FunctionAddresses, error) {
			got = fns
			return nil, nil
		},
	}

	// Three streams, all declaring the same single Function.
	streams := []*parse.Stream{
		parseStream(t, completeStream),
		parseStream(t, completeStream),
		parseStream(t, completeStream),
	}

	if _, errs := r.RenderAll(context.Background(), streams, testDomain); errs[0] != nil {
		t.Fatalf("RenderAll(...): unexpected error: %v", errs[0])
	}

	if len(got) != 1 {
		names := make([]string, 0, len(got))
		for _, fn := range got {
			names = append(names, fn.GetName())
		}
		t.Errorf("RenderAll(...): started %d functions %v, want 1 after dedupe", len(got), names)
	}
}

// Render must remain a thin wrapper over RenderAll so the two paths cannot drift.
func TestRenderDelegatesToRenderAll(t *testing.T) {
	renders := 0
	eng := &render.MockEngine{
		MockRender: func(ctx context.Context, req *renderv1alpha1.RenderRequest) (*renderv1alpha1.RenderResponse, error) {
			renders++
			return (&render.MockEngine{}).Render(ctx, req)
		},
	}

	res, err := testRenderer(t, eng).Render(context.Background(), parseStream(t, completeStream), testDomain)
	if err != nil {
		t.Fatalf("Render(...): unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("Render(...): want a result, got nil")
	}
	if renders != 1 {
		t.Errorf("Render(...): engine rendered %d times, want 1", renders)
	}
}
