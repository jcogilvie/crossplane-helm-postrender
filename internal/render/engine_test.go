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

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"

	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"

	"github.com/crossplane/cli/v2/cmd/crossplane/render"
	renderv1alpha1 "github.com/crossplane/cli/v2/proto/render/v1alpha1"

	"github.com/jcogilvie/crossplane-helm-postrender/internal/parse"
)

const testDomain = "platform.example.org"

// errBoom is a sentinel used to force engine failures in tests.
var errBoom = errors.New("boom")

// A complete, minimal input stream. Each test mutates a copy of this rather than
// redeclaring the whole thing.
const completeStream = `#### file: chart/templates/xr.yaml
apiVersion: platform.example.org/v1alpha1
kind: XBucket
metadata:
  name: my-xr
---
#### file: chart/templates/composition.yaml
apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: xbucket
spec:
  mode: Pipeline
  pipeline:
  - step: go-templating
    functionRef:
      name: function-go-templating
---
#### file: chart/templates/functions.yaml
apiVersion: pkg.crossplane.io/v1
kind: Function
metadata:
  name: function-go-templating
spec:
  package: xpkg.crossplane.io/crossplane-contrib/function-go-templating:v0.12.2
`

// testRenderer wires a renderer to a MockEngine so these tests need no Docker.
// startRuntimes returns nil, which StopFunctionRuntimes explicitly tolerates.
func testRenderer(t *testing.T, eng *render.MockEngine) *engineRenderer {
	t.Helper()
	return &engineRenderer{
		o:         Options{}.withDefaults(),
		newEngine: func() render.Engine { return eng },
		startRuntimes: func(_ context.Context, _ []pkgv1.Function) (*render.FunctionAddresses, error) {
			return nil, nil
		},
	}
}

// parseStream accepts testing.TB so benchmarks can use it too.
func parseStream(tb testing.TB, in string) *parse.Stream {
	tb.Helper()
	s, err := parse.Parse(strings.NewReader(in), parse.Options{APIGroupDomain: testDomain})
	if err != nil {
		tb.Fatalf("parse.Parse(...): unexpected error: %v", err)
	}
	return s
}

// The MockEngine's default Render echoes the request's composite back, so a
// successful call proves the inputs converted and the request was built.
func TestRenderConvertsInputsAndReturnsComposite(t *testing.T) {
	r := testRenderer(t, &render.MockEngine{})

	got, err := r.Render(context.Background(), parseStream(t, completeStream), testDomain)
	if err != nil {
		t.Fatalf("Render(...): unexpected error: %v", err)
	}
	if got.Composite == nil {
		t.Fatal("Render(...): want a composite in the result, got nil")
	}
	if name := got.Composite.GetName(); name != "my-xr" {
		t.Errorf("Render(...): composite name = %q, want %q", name, "my-xr")
	}
}

// Validation must run before any engine work, so a caller gets an error naming
// the missing input rather than an obscure failure from deeper in -- and so a
// malformed stream does not pay the cost of starting Docker containers.
func TestRenderValidatesBeforeRendering(t *testing.T) {
	tests := map[string]struct {
		in      string
		wantErr string
	}{
		"MissingXR": {
			in: `apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: c
---
apiVersion: pkg.crossplane.io/v1
kind: Function
metadata:
  name: f
`,
			wantErr: "no XR found",
		},
		"MissingComposition": {
			in: `apiVersion: platform.example.org/v1alpha1
kind: XBucket
metadata:
  name: xr
---
apiVersion: pkg.crossplane.io/v1
kind: Function
metadata:
  name: f
`,
			wantErr: "no Composition found",
		},
		"MissingFunctions": {
			in: `apiVersion: platform.example.org/v1alpha1
kind: XBucket
metadata:
  name: xr
---
apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: c
`,
			wantErr: "no Functions found",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// A engine that fails loudly if reached: validation must short-circuit.
			eng := &render.MockEngine{
				MockSetup: func(_ context.Context, _ []pkgv1.Function) (func(), error) {
					t.Error("Render(...): engine Setup called despite invalid input")
					return func() {}, nil
				},
			}

			_, err := testRenderer(t, eng).Render(context.Background(), parseStream(t, tt.in), testDomain)
			if err == nil {
				t.Fatalf("Render(...): want error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Render(...): want error containing %q, got %q", tt.wantErr, err)
			}
		})
	}
}

// The engine's cleanup must run even when Render fails, or Docker resources leak
// across a suite.
func TestRenderRunsCleanupOnRenderFailure(t *testing.T) {
	cleaned := false
	eng := &render.MockEngine{
		MockSetup: func(_ context.Context, _ []pkgv1.Function) (func(), error) {
			return func() { cleaned = true }, nil
		},
		MockRender: func(_ context.Context, _ *renderv1alpha1.RenderRequest) (*renderv1alpha1.RenderResponse, error) {
			return nil, errBoom
		},
	}

	if _, err := testRenderer(t, eng).Render(context.Background(), parseStream(t, completeStream), testDomain); err == nil {
		t.Fatal("Render(...): want error from the engine, got nil")
	}
	if !cleaned {
		t.Error("Render(...): engine cleanup was not called after a render failure")
	}
}

func TestResultManifestsOrdersCompositeFirst(t *testing.T) {
	r := testRenderer(t, &render.MockEngine{})

	got, err := r.Render(context.Background(), parseStream(t, completeStream), testDomain)
	if err != nil {
		t.Fatalf("Render(...): unexpected error: %v", err)
	}

	ms := got.Manifests()
	if len(ms) == 0 {
		t.Fatal("Manifests(): want at least the composite, got none")
	}
	if kind := ms[0].GetKind(); kind != "XBucket" {
		t.Errorf("Manifests()[0] kind = %q, want the composite %q first", kind, "XBucket")
	}
}

// Output must be a document stream with a leading separator, matching the bash
// renderer byte-for-byte -- the suites parse this.
func TestResultYAMLFormat(t *testing.T) {
	r := testRenderer(t, &render.MockEngine{})

	res, err := r.Render(context.Background(), parseStream(t, completeStream), testDomain)
	if err != nil {
		t.Fatalf("Render(...): unexpected error: %v", err)
	}

	out, err := res.YAML()
	if err != nil {
		t.Fatalf("YAML(): unexpected error: %v", err)
	}
	if !strings.HasPrefix(string(out), "---\n") {
		t.Errorf("YAML(): want output to start with %q, got %q", "---\n", first(string(out), 20))
	}
	if !strings.Contains(string(out), "kind: XBucket") {
		t.Error("YAML(): want the composite in the output")
	}
}

func first(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
