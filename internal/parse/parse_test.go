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
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

const testDomain = "platform.example.org"

func TestParseClassifies(t *testing.T) {
	tests := map[string]struct {
		in   string
		want map[Class]int
	}{
		"XRByAPIGroup": {
			in: `apiVersion: platform.example.org/v1alpha1
kind: XBucket
metadata:
  name: xr
`,
			want: map[Class]int{ClassXR: 1},
		},
		"CompositionFunctionXRDEnvConfig": {
			in: `apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: c
---
apiVersion: pkg.crossplane.io/v1
kind: Function
metadata:
  name: f1
---
apiVersion: pkg.crossplane.io/v1
kind: Function
metadata:
  name: f2
---
apiVersion: apiextensions.crossplane.io/v2
kind: CompositeResourceDefinition
metadata:
  name: x
---
apiVersion: apiextensions.crossplane.io/v1beta1
kind: EnvironmentConfig
metadata:
  name: e
`,
			want: map[Class]int{
				ClassComposition: 1, ClassFunction: 2, ClassXRD: 1, ClassEnvironmentConfig: 1,
			},
		},
		"UnrelatedKindIsUnknown": {
			in: `apiVersion: v1
kind: ConfigMap
metadata:
  name: cm
`,
			want: map[Class]int{ClassUnknown: 1},
		},
		"EmptyAndCommentOnlyDocumentsSkipped": {
			in: `---
# just a comment

---
apiVersion: platform.example.org/v1alpha1
kind: XBucket
metadata:
  name: xr
---

`,
			want: map[Class]int{ClassXR: 1},
		},
		// The test-injection annotations must win over kind/apiVersion routing,
		// or an annotated XR would overwrite the XR being rendered.
		"ObservedAnnotationBeatsXRRouting": {
			in: `apiVersion: platform.example.org/v1alpha1
kind: XBucket
metadata:
  name: observed-xr
  annotations:
    crossplane.io/test-observed: "true"
`,
			want: map[Class]int{ClassObserved: 1},
		},
		"ExtraResourceAnnotationBeatsXRRouting": {
			in: `apiVersion: platform.example.org/v1alpha1
kind: AWSSecret
metadata:
  name: extra
  annotations:
    crossplane.io/test-extra-resource: "true"
`,
			// Extra-resources share the EnvironmentConfig bucket.
			want: map[Class]int{ClassEnvironmentConfig: 1},
		},
		// grep 'test-observed: "true"' would also match a value of "false" on a
		// neighbouring line; parsing the value cannot.
		"FalseAnnotationIsNotObserved": {
			in: `apiVersion: platform.example.org/v1alpha1
kind: XBucket
metadata:
  name: xr
  annotations:
    crossplane.io/test-observed: "false"
`,
			want: map[Class]int{ClassXR: 1},
		},
		// A "---" inside a block scalar is data, not a separator. The bash
		// renderer's line-oriented scan split here and corrupted the document.
		"SeparatorInsideBlockScalarDoesNotSplit": {
			in: `apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: c
spec:
  pipeline:
  - step: go-templating
    input:
      inline:
        template: |
          ---
          apiVersion: v1
          kind: ConfigMap
`,
			want: map[Class]int{ClassComposition: 1},
		},
		"MalformedDocumentIsUnknownNotFatal": {
			in: `this: is: not: valid: yaml:
  - [unbalanced
`,
			want: map[Class]int{ClassUnknown: 1},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s, err := Parse(strings.NewReader(tt.in), Options{APIGroupDomain: testDomain})
			if err != nil {
				t.Fatalf("Parse(...): unexpected error: %v", err)
			}

			got := map[Class]int{}
			if s.XR != nil {
				got[ClassXR]++
			}
			if s.Composition != nil {
				got[ClassComposition]++
			}
			for c, n := range map[Class]int{
				ClassFunction:          len(s.Functions),
				ClassXRD:               len(s.XRDs),
				ClassEnvironmentConfig: len(s.EnvironmentConfigs),
				ClassObserved:          len(s.Observed),
				ClassUnknown:           len(s.Unknown),
			} {
				if n > 0 {
					got[c] = n
				}
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Parse(...): -want +got:\n%s", diff)
			}
		})
	}
}

func TestParseRecordsSource(t *testing.T) {
	tests := map[string]struct {
		in   string
		want string
	}{
		"UnittestMarker": {
			in: `#### file: chart/templates/xr.yaml
apiVersion: platform.example.org/v1alpha1
kind: XBucket
metadata:
  name: xr
`,
			want: "chart/templates/xr.yaml",
		},
		"HelmTemplateMarker": {
			in: `# Source: chart/templates/xr.yaml
apiVersion: platform.example.org/v1alpha1
kind: XBucket
metadata:
  name: xr
`,
			want: "chart/templates/xr.yaml",
		},
		// helm v4's plugin path emits no marker, only this annotation.
		"V4FilenameAnnotation": {
			in: `apiVersion: platform.example.org/v1alpha1
kind: XBucket
metadata:
  name: xr
  annotations:
    postrenderer.helm.sh/postrender-filename: 'chart/templates/xr.yaml'
`,
			want: "chart/templates/xr.yaml",
		},
		"NoProvenance": {
			in: `apiVersion: platform.example.org/v1alpha1
kind: XBucket
metadata:
  name: xr
`,
			want: "",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s, err := Parse(strings.NewReader(tt.in), Options{APIGroupDomain: testDomain})
			if err != nil {
				t.Fatalf("Parse(...): unexpected error: %v", err)
			}
			if s.XR == nil {
				t.Fatalf("Parse(...): expected an XR, got none")
			}
			if diff := cmp.Diff(tt.want, s.XR.Source); diff != "" {
				t.Errorf("Parse(...).XR.Source: -want +got:\n%s", diff)
			}
		})
	}
}

// Later XRs and Compositions overwrite earlier ones, matching the bash
// renderer's `cat > $FILE` behaviour.
func TestParseLastXRWins(t *testing.T) {
	in := `apiVersion: platform.example.org/v1alpha1
kind: XBucket
metadata:
  name: first
---
apiVersion: platform.example.org/v1alpha1
kind: XBucket
metadata:
  name: second
`
	s, err := Parse(strings.NewReader(in), Options{APIGroupDomain: testDomain})
	if err != nil {
		t.Fatalf("Parse(...): unexpected error: %v", err)
	}
	if got := s.XR.Object.GetName(); got != "second" {
		t.Errorf("Parse(...).XR name: want %q, got %q", "second", got)
	}
}

func TestParseRequiresAPIGroupDomain(t *testing.T) {
	if _, err := Parse(strings.NewReader("kind: Composition\n"), Options{}); err == nil {
		t.Error("Parse(...) with no API group domain: want error, got nil")
	}
}

func TestValidate(t *testing.T) {
	xr := &Document{}
	comp := &Document{}
	fns := []Document{{}}

	tests := map[string]struct {
		s       Stream
		wantErr string
	}{
		"Complete":           {s: Stream{XR: xr, Composition: comp, Functions: fns}},
		"MissingXR":          {s: Stream{Composition: comp, Functions: fns}, wantErr: "no XR found"},
		"MissingComposition": {s: Stream{XR: xr, Functions: fns}, wantErr: "no Composition found"},
		"MissingFunctions":   {s: Stream{XR: xr, Composition: comp}, wantErr: "no Functions found"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := tt.s.Validate(testDomain)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate(): unexpected error: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Errorf("Validate(): want error containing %q, got nil", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Errorf("Validate(): want error containing %q, got %q", tt.wantErr, err)
			}
		})
	}
}
