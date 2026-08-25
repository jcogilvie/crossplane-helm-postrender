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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// xrdStream parses YAML and returns the XRD documents, so these tests exercise
// the real parse path rather than hand-built objects.
func xrdStream(t *testing.T, in string) *Stream {
	t.Helper()
	s, err := Parse(strings.NewReader(in), Options{APIGroupDomain: testDomain})
	if err != nil {
		t.Fatalf("Parse(...): unexpected error: %v", err)
	}
	return s
}

func TestMatchXRD(t *testing.T) {
	// Two XRDs, each naming a composite kind and a claim kind. The first XRD
	// deliberately comes before the match so a bug that returns the first XRD
	// unconditionally is caught.
	const xrds = `apiVersion: apiextensions.crossplane.io/v2
kind: CompositeResourceDefinition
metadata:
  name: xbuckets
spec:
  group: platform.example.org
  names:
    kind: XBucket
    plural: xbuckets
  claimNames:
    kind: BucketClaim
    plural: bucketclaims
---
apiVersion: apiextensions.crossplane.io/v2
kind: CompositeResourceDefinition
metadata:
  name: xqueues
spec:
  group: platform.example.org
  names:
    kind: XQueue
    plural: xqueues
  claimNames:
    kind: QueueClaim
    plural: queueclaims
`

	tests := map[string]struct {
		xrKind   string
		wantName string // XRD metadata.name, or "" for no match
	}{
		"MatchesCompositeKind":      {xrKind: "XQueue", wantName: "xqueues"},
		"MatchesFirstCompositeKind": {xrKind: "XBucket", wantName: "xbuckets"},
		"MatchesClaimKind":          {xrKind: "QueueClaim", wantName: "xqueues"},
		"NoMatchForUnknownKind":     {xrKind: "XDatabase", wantName: ""},
		// A kind that is a substring of a real one must not match: the bash
		// version's grep-based comparison was vulnerable to this before -qFx.
		"NoSubstringMatch":     {xrKind: "Queue", wantName: ""},
		"NoMatchForPluralName": {xrKind: "xqueues", wantName: ""},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := xrdStream(t, xrds)

			xr := xrStream(t, tt.xrKind)
			got := MatchXRD(xr, s.XRDs)

			switch {
			case tt.wantName == "" && got != nil:
				t.Errorf("MatchXRD(%q): want no match, got %q", tt.xrKind, got.Object.GetName())
			case tt.wantName != "" && got == nil:
				t.Errorf("MatchXRD(%q): want %q, got no match", tt.xrKind, tt.wantName)
			case tt.wantName != "" && got.Object.GetName() != tt.wantName:
				t.Errorf("MatchXRD(%q): want %q, got %q", tt.xrKind, tt.wantName, got.Object.GetName())
			}
		})
	}
}

// xrStream builds an XR of the given kind through the parser.
func xrStream(t *testing.T, kind string) *unstructured.Unstructured {
	t.Helper()
	in := "apiVersion: platform.example.org/v1alpha1\nkind: " + kind + "\nmetadata:\n  name: xr\n"
	s, err := Parse(strings.NewReader(in), Options{APIGroupDomain: testDomain})
	if err != nil {
		t.Fatalf("Parse(...): unexpected error: %v", err)
	}
	if s.XR == nil {
		t.Fatalf("Parse(...): expected an XR for kind %q, got none", kind)
	}
	return s.XR.Object
}

func TestMatchXRDEdgeCases(t *testing.T) {
	t.Run("NilXR", func(t *testing.T) {
		if got := MatchXRD(nil, nil); got != nil {
			t.Errorf("MatchXRD(nil, nil): want nil, got %v", got)
		}
	})

	t.Run("NoXRDs", func(t *testing.T) {
		if got := MatchXRD(xrStream(t, "XBucket"), nil); got != nil {
			t.Errorf("MatchXRD(xr, nil): want nil, got %v", got)
		}
	})

	// An XRD with no names block must be skipped rather than panic.
	t.Run("XRDWithoutNames", func(t *testing.T) {
		s := xrdStream(t, `apiVersion: apiextensions.crossplane.io/v2
kind: CompositeResourceDefinition
metadata:
  name: malformed
spec:
  group: platform.example.org
`)
		if got := MatchXRD(xrStream(t, "XBucket"), s.XRDs); got != nil {
			t.Errorf("MatchXRD(...): want nil for XRD with no names, got %v", got.Object.GetName())
		}
	})
}
