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
	"maps"
	"regexp"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"

	"github.com/crossplane/cli/v2/cmd/crossplane/render"

	"github.com/jcogilvie/crossplane-helm-postrender/internal/parse"
)

// fn builds a Function with a name, package reference, and optional annotations.
func fn(name, pkg string, annotations map[string]string) pkgv1.Function {
	f := pkgv1.Function{}
	f.SetName(name)
	f.Spec.Package = pkg
	if annotations != nil {
		f.SetAnnotations(annotations)
	}
	return f
}

func TestEnableContainerReuse(t *testing.T) {
	const pkg = "xpkg.crossplane.io/crossplane-contrib/function-go-templating:v0.12.2"

	tests := map[string]struct {
		fns      []pkgv1.Function
		suffix   string
		wantName string
		// wantUntouched asserts the Function keeps exactly the annotations it
		// started with.
		wantUntouched bool
	}{
		"DerivesNameFromFunctionAndVersion": {
			fns:      []pkgv1.Function{fn("function-go-templating", pkg, nil)},
			wantName: "function-go-templating-v0.12.2-" + DefaultReuseSuffix,
		},
		"CustomSuffix": {
			fns:      []pkgv1.Function{fn("function-go-templating", pkg, nil)},
			suffix:   "myproject",
			wantName: "function-go-templating-v0.12.2-myproject",
		},
		"NoPackageReference": {
			fns:      []pkgv1.Function{fn("function-bare", "", nil)},
			wantName: "function-bare-" + DefaultReuseSuffix,
		},
		"PackageWithoutTag": {
			fns:      []pkgv1.Function{fn("function-untagged", "xpkg.crossplane.io/org/function-untagged", nil)},
			wantName: "function-untagged-" + DefaultReuseSuffix,
		},
		// A digest reference contains a ":" and a "@", and Docker rejects "@".
		"DigestReferenceIsSanitised": {
			fns: []pkgv1.Function{fn("function-digest",
				"xpkg.crossplane.io/org/function-digest@sha256:abc123", nil)},
			wantName: "function-digest-sha256-abc123-" + DefaultReuseSuffix,
		},
		// An explicit choice in a chart must win, or this would silently override
		// someone's deliberate configuration.
		"ExplicitNameIsRespected": {
			fns: []pkgv1.Function{fn("function-go-templating", pkg, map[string]string{
				annotationContainerName: "my-own-container",
			})},
			wantUntouched: true,
		},
		"ExplicitCleanupIsRespected": {
			fns: []pkgv1.Function{fn("function-go-templating", pkg, map[string]string{
				annotationCleanup: "Remove",
			})},
			wantUntouched: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			before := maps.Clone(tt.fns[0].GetAnnotations())
			if before == nil {
				before = map[string]string{}
			}

			if err := enableContainerReuse(tt.fns, tt.suffix); err != nil {
				t.Fatalf("enableContainerReuse(...): unexpected error: %v", err)
			}

			got := tt.fns[0].GetAnnotations()

			if tt.wantUntouched {
				if diff := cmp.Diff(before, got); diff != "" {
					t.Errorf("enableContainerReuse(...): annotations changed, -want +got:\n%s", diff)
				}
				return
			}

			if diff := cmp.Diff(tt.wantName, got[annotationContainerName]); diff != "" {
				t.Errorf("container name: -want +got:\n%s", diff)
			}
			if diff := cmp.Diff(cleanupOrphan, got[annotationCleanup]); diff != "" {
				t.Errorf("cleanup policy: -want +got:\n%s", diff)
			}
		})
	}
}

// Every derived name must be something Docker will actually accept, or the render
// fails at container creation with a message that says nothing about annotations.
func TestContainerNamesAreValidForDocker(t *testing.T) {
	// Docker's own constraint: [a-zA-Z0-9][a-zA-Z0-9_.-]*
	valid := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

	fns := []pkgv1.Function{
		fn("function-go-templating", "xpkg.crossplane.io/org/f:v0.12.2", nil),
		fn("function-digest", "xpkg.crossplane.io/org/f@sha256:abc", nil),
		fn("weird.name_here", "reg/f:v1.0.0+build.5", nil),
		fn("-leading-dash", "reg/f:1.0", nil),
		fn("f", "", nil),
	}

	if err := enableContainerReuse(fns, DefaultReuseSuffix); err != nil {
		t.Fatalf("enableContainerReuse(...): unexpected error: %v", err)
	}

	for i := range fns {
		name := fns[i].GetAnnotations()[annotationContainerName]
		if !valid.MatchString(name) {
			t.Errorf("Function %q produced container name %q, which Docker would reject",
				fns[i].GetName(), name)
		}
	}
}

// Reuse only works if the same Function yields the same name every run.
func TestContainerNameIsStable(t *testing.T) {
	const pkg = "xpkg.crossplane.io/org/function-go-templating:v0.12.2"

	first := []pkgv1.Function{fn("function-go-templating", pkg, nil)}
	second := []pkgv1.Function{fn("function-go-templating", pkg, nil)}

	for _, fns := range [][]pkgv1.Function{first, second} {
		if err := enableContainerReuse(fns, ""); err != nil {
			t.Fatalf("enableContainerReuse(...): unexpected error: %v", err)
		}
	}

	a := first[0].GetAnnotations()[annotationContainerName]
	b := second[0].GetAnnotations()[annotationContainerName]
	if a != b {
		t.Errorf("container name is not stable across runs: %q vs %q", a, b)
	}
}

// Distinct Functions must not collide, or they would fight over one container.
func TestContainerNamesDoNotCollide(t *testing.T) {
	fns := []pkgv1.Function{
		fn("function-go-templating", "reg/f:v0.12.2", nil),
		fn("function-go-templating", "reg/f:v0.13.0", nil), // same fn, newer version
		fn("function-auto-ready", "reg/f:v0.12.2", nil),    // different fn, same version
	}

	if err := enableContainerReuse(fns, DefaultReuseSuffix); err != nil {
		t.Fatalf("enableContainerReuse(...): unexpected error: %v", err)
	}

	seen := map[string]string{}
	for i := range fns {
		name := fns[i].GetAnnotations()[annotationContainerName]
		if prev, dup := seen[name]; dup {
			t.Errorf("container name %q derived for both %q and %q", name, prev, fns[i].GetName())
		}
		seen[name] = fns[i].GetName()
	}
}

// The annotations have to be applied to the Functions the engine and the runtime
// starter actually receive. They are handed the same deduped slice, so a copy made
// somewhere in between would silently drop reuse.
func TestRenderAllAppliesReuseToStartedFunctions(t *testing.T) {
	var setupFns, startedFns []pkgv1.Function

	eng := &render.MockEngine{
		MockSetup: func(_ context.Context, fns []pkgv1.Function) (func(), error) {
			setupFns = fns
			return func() {}, nil
		},
	}

	r := &engineRenderer{
		o: Options{ReuseContainers: true}.withDefaults(),
		newEngine: func() render.Engine {
			return eng
		},
		startRuntimes: func(_ context.Context, fns []pkgv1.Function) (*render.FunctionAddresses, error) {
			startedFns = fns
			return nil, nil
		},
	}

	if _, errs := r.RenderAll(context.Background(),
		[]*parse.Stream{parseStream(t, completeStream)}, testDomain); errs[0] != nil {
		t.Fatalf("RenderAll(...): unexpected error: %v", errs[0])
	}

	for _, tc := range []struct {
		what string
		fns  []pkgv1.Function
	}{{"Setup", setupFns}, {"startRuntimes", startedFns}} {
		if len(tc.fns) == 0 {
			t.Fatalf("%s received no functions", tc.what)
		}
		anns := tc.fns[0].GetAnnotations()
		if anns[annotationCleanup] != cleanupOrphan {
			t.Errorf("%s: cleanup annotation = %q, want %q",
				tc.what, anns[annotationCleanup], cleanupOrphan)
		}
		if !strings.HasSuffix(anns[annotationContainerName], DefaultReuseSuffix) {
			t.Errorf("%s: container name %q does not carry the reuse suffix",
				tc.what, anns[annotationContainerName])
		}
	}
}

// Off by default: a single render must not leave containers behind.
func TestRenderAllWithoutReuseLeavesAnnotationsAlone(t *testing.T) {
	var startedFns []pkgv1.Function

	r := &engineRenderer{
		o:         Options{}.withDefaults(),
		newEngine: func() render.Engine { return &render.MockEngine{} },
		startRuntimes: func(_ context.Context, fns []pkgv1.Function) (*render.FunctionAddresses, error) {
			startedFns = fns
			return nil, nil
		},
	}

	if _, errs := r.RenderAll(context.Background(),
		[]*parse.Stream{parseStream(t, completeStream)}, testDomain); errs[0] != nil {
		t.Fatalf("RenderAll(...): unexpected error: %v", errs[0])
	}

	if len(startedFns) == 0 {
		t.Fatal("startRuntimes received no functions")
	}
	anns := startedFns[0].GetAnnotations()
	if _, ok := anns[annotationCleanup]; ok {
		t.Errorf("cleanup annotation set without ReuseContainers: %q", anns[annotationCleanup])
	}
	if _, ok := anns[annotationContainerName]; ok {
		t.Errorf("container name set without ReuseContainers: %q", anns[annotationContainerName])
	}
}

// An invalid network name must be rejected before it reaches a subprocess, with a
// message that says what was wrong rather than surfacing a Docker error.
func TestEnsureNetworkRejectsInvalidNames(t *testing.T) {
	for _, name := range []string{"-leading-dash", "has spaces", "semi;colon", "$(subshell)"} {
		if err := ensureNetwork(context.Background(), name); err == nil {
			t.Errorf("ensureNetwork(%q): want an error, got nil", name)
		}
	}
}

// An empty name means "let the engine manage its own network", which is not an
// error -- it is the default when reuse is off.
func TestEnsureNetworkAcceptsEmpty(t *testing.T) {
	if err := ensureNetwork(context.Background(), ""); err != nil {
		t.Errorf("ensureNetwork(\"\"): unexpected error: %v", err)
	}
}
