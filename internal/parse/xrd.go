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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// MatchXRD returns the XRD that defines the XR's kind, or nil if none does.
//
// An XRD names two kinds: spec.names.kind (the composite) and
// spec.claimNames.kind (the claim). Either may match, so both are checked.
//
// Fields are read via unstructured.NestedString rather than by decoding into a
// typed CompositeResourceDefinition. The apiserver round-trips XRDs through
// v1<->v2 conversion, which makes the document's own apiVersion unreliable, so
// typed decoding against a guessed version can silently drop fields. Reading the
// paths directly sidesteps that -- the same approach crossplane-diff takes in
// client/crossplane/definition_client.go.
//
// Matching structurally also avoids two failure modes that text matching has
// here. Because an XRD names two kinds, a text search returns both and must then
// compare against a multi-line result -- which is an exact-match trap -- and any
// windowed search (`grep -A N`) silently stops working when the two keys fall
// further apart than the window.
func MatchXRD(xr *unstructured.Unstructured, xrds []Document) *Document {
	if xr == nil {
		return nil
	}

	kind := xr.GetKind()
	if kind == "" {
		return nil
	}

	for i := range xrds {
		obj := xrds[i].Object
		if obj == nil {
			continue
		}
		for _, path := range [][]string{
			{"spec", "names", "kind"},
			{"spec", "claimNames", "kind"},
		} {
			// Ignore the error: a missing or non-string field simply does not
			// match, which is not an error condition here.
			if v, ok, _ := unstructured.NestedString(obj.Object, path...); ok && v == kind {
				return &xrds[i]
			}
		}
	}

	return nil
}
