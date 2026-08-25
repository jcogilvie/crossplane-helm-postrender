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
	"testing"
	"time"

	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"

	"github.com/crossplane/cli/v2/cmd/crossplane/render"

	"github.com/jcogilvie/crossplane-helm-postrender/internal/parse"
)

// setupCost approximates what engine and runtime setup actually costs against
// Docker. Measured per-invocation overhead is on the order of a second; 20ms
// keeps the benchmark quick while preserving the ratio being demonstrated.
const setupCost = 20 * time.Millisecond

// slowSetupRenderer models the real cost structure: setup is expensive, the
// render itself is cheap.
func slowSetupRenderer() *engineRenderer {
	return &engineRenderer{
		o: Options{}.withDefaults(),
		newEngine: func() render.Engine {
			return &render.MockEngine{
				MockSetup: func(_ context.Context, _ []pkgv1.Function) (func(), error) {
					time.Sleep(setupCost)
					return func() {}, nil
				},
			}
		},
		startRuntimes: func(_ context.Context, _ []pkgv1.Function) (*render.FunctionAddresses, error) {
			time.Sleep(setupCost)
			return nil, nil
		},
	}
}

// BenchmarkRenderPerStream is the status quo: one process, one setup, one render
// -- what Helm's post-renderer contract forces, repeated per test case.
func BenchmarkRenderPerStream(b *testing.B) {
	streams := benchStreams(b, 10)
	r := slowSetupRenderer()

	for b.Loop() {
		for _, s := range streams {
			if _, err := r.Render(context.Background(), s, testDomain); err != nil {
				b.Fatalf("Render(...): unexpected error: %v", err)
			}
		}
	}
}

// BenchmarkRenderAllBatched renders the same streams through one shared
// environment. The delta is the setup cost that per-stream rendering pays
// repeatedly and this pays once.
func BenchmarkRenderAllBatched(b *testing.B) {
	streams := benchStreams(b, 10)
	r := slowSetupRenderer()

	for b.Loop() {
		_, errs := r.RenderAll(context.Background(), streams, testDomain)
		for i := range errs {
			if errs[i] != nil {
				b.Fatalf("RenderAll(...): stream %d: unexpected error: %v", i, errs[i])
			}
		}
	}
}

func benchStreams(b *testing.B, n int) []*parse.Stream {
	b.Helper()
	out := make([]*parse.Stream, n)
	for i := range out {
		out[i] = parseStream(b, completeStream)
	}
	return out
}
