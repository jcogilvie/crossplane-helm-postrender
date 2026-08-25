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

package parse

import (
	"os"
	"path/filepath"
	"testing"
)

// These fixtures cover the two stream shapes a post-renderer actually receives:
//
//   - helm-template-stream.yaml -- what `helm template` produces, with `# Source:`
//     provenance comments between documents.
//   - helm-v4-plugin-stream.yaml -- what Helm v4 hands a postrenderer/v1 plugin:
//     a bare `---` between documents and provenance in an annotation instead.
//
// Both are more realistic than the unit-test inputs: multiple Functions, an
// EnvironmentConfig, an unrelated ConfigMap that must be ignored, and a
// Composition whose inline template contains a `---` inside a block scalar.
func TestParseStreamShapes(t *testing.T) {
	tests := map[string]struct {
		file string
		// Exact counts: these fixtures are authored, so any change in what the
		// classifier routes should be deliberate and visible here.
		wantXR          bool
		wantComposition bool
		wantFns         int
		wantXRDs        int
		wantEnvConfigs  int
		wantUnknown     int
	}{
		"HelmTemplateStream": {
			file:            "helm-template-stream.yaml",
			wantXR:          true,
			wantComposition: true,
			wantFns:         3,
			wantXRDs:        1,
			wantEnvConfigs:  1,
			// The unrelated ConfigMap, which must not be routed anywhere.
			wantUnknown: 1,
		},
		"HelmV4PluginStream": {
			file:            "helm-v4-plugin-stream.yaml",
			wantXR:          true,
			wantComposition: true,
			wantFns:         2,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata", tt.file))
			if err != nil {
				t.Fatalf("cannot open fixture: %v", err)
			}
			defer f.Close()

			s, err := Parse(f, Options{APIGroupDomain: testDomain})
			if err != nil {
				t.Fatalf("Parse(%s): unexpected error: %v", tt.file, err)
			}

			if got := s.XR != nil; got != tt.wantXR {
				t.Errorf("Parse(%s): XR found = %v, want %v", tt.file, got, tt.wantXR)
			}
			if got := s.Composition != nil; got != tt.wantComposition {
				t.Errorf("Parse(%s): Composition found = %v, want %v", tt.file, got, tt.wantComposition)
			}
			if got := len(s.Functions); got != tt.wantFns {
				t.Errorf("Parse(%s): Functions = %d, want %d", tt.file, got, tt.wantFns)
			}
			if got := len(s.XRDs); got != tt.wantXRDs {
				t.Errorf("Parse(%s): XRDs = %d, want %d", tt.file, got, tt.wantXRDs)
			}
			if got := len(s.EnvironmentConfigs); got != tt.wantEnvConfigs {
				t.Errorf("Parse(%s): EnvironmentConfigs = %d, want %d", tt.file, got, tt.wantEnvConfigs)
			}
			if got := len(s.Unknown); got != tt.wantUnknown {
				t.Errorf("Parse(%s): Unknown = %d, want %d", tt.file, got, tt.wantUnknown)
			}

			// Every stream must satisfy the render preconditions; if it does
			// not, the classifier dropped something it should have routed.
			if err := s.Validate(testDomain); err != nil {
				t.Errorf("Parse(%s): Validate() failed: %v", tt.file, err)
			}

			// Provenance must survive for every routed document, since that is
			// what lets output be attributed back to a template.
			for _, d := range append(append([]Document{}, s.Functions...), s.XRDs...) {
				if d.Source == "" {
					t.Errorf("Parse(%s): document %s/%s has no Source",
						tt.file, d.Object.GetKind(), d.Object.GetName())
				}
			}
		})
	}
}

// The XR in the helm-template fixture must resolve to its XRD, exercising
// MatchXRD against a full stream rather than a hand-built pair.
func TestMatchXRDAgainstStream(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "helm-template-stream.yaml"))
	if err != nil {
		t.Fatalf("cannot open fixture: %v", err)
	}
	defer f.Close()

	s, err := Parse(f, Options{APIGroupDomain: testDomain})
	if err != nil {
		t.Fatalf("Parse(...): unexpected error: %v", err)
	}

	xrd := MatchXRD(s.XR.Object, s.XRDs)
	if xrd == nil {
		t.Fatalf("MatchXRD(...): no XRD matched XR kind %q", s.XR.Object.GetKind())
	}
	t.Logf("XR kind %q matched XRD %q", s.XR.Object.GetKind(), xrd.Object.GetName())
}
