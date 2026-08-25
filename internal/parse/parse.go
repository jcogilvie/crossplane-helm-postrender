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

// Package parse splits a post-renderer input stream into typed buckets that
// mirror the arguments `crossplane render` expects.
//
// Classification works on parsed objects rather than on the first line of text
// that happens to match. That distinction matters more than it sounds: shell
// implementations of this job typically grep for `kind:` and route on the first
// hit, which misclassifies any document whose first `kind:` belongs to a nested
// field, and split on any `---` even inside a block scalar -- which compositions
// contain routinely, since function-go-templating inlines YAML templates.
package parse

import (
	"bufio"
	"bytes"
	"io"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
)

// Kubernetes kinds that route a document to a specific render input.
const (
	kindComposition = "Composition"
	kindFunction    = "Function"
	kindXRD         = "CompositeResourceDefinition"
	kindEnvConfig   = "EnvironmentConfig"
)

// Annotations that let a test inject synthetic render inputs.
//
// testObserved routes a manifest to --observed-resources, so tests can supply
// composed-resource status (e.g. to exercise branches guarded by
// getComposedResource). testExtraResource routes to --extra-resources, so
// go-templating ExtraResources requirements resolve against synthetic objects.
const (
	annotationTestObserved      = "crossplane.io/test-observed"
	annotationTestExtraResource = "crossplane.io/test-extra-resource"
)

// Class identifies which render input a document belongs to.
type Class int

// Document classes, in the order the classifier checks them.
const (
	// ClassUnknown is a document that matches no other class. It is dropped
	// rather than treated as an error, so an unrelated manifest in the stream
	// cannot fail a render.
	ClassUnknown Class = iota
	ClassXR
	ClassComposition
	ClassFunction
	ClassXRD
	ClassEnvironmentConfig
	ClassObserved
	ClassExtraResource
)

// String returns the class name, for logs and test failure messages.
func (c Class) String() string {
	switch c {
	case ClassXR:
		return "XR"
	case ClassComposition:
		return "Composition"
	case ClassFunction:
		return "Function"
	case ClassXRD:
		return "XRD"
	case ClassEnvironmentConfig:
		return "EnvironmentConfig"
	case ClassObserved:
		return "Observed"
	case ClassExtraResource:
		return "ExtraResource"
	case ClassUnknown:
		return "Unknown"
	default:
		return "Unknown"
	}
}

// Document is one parsed YAML document plus the provenance we can recover for
// it. Raw is retained verbatim so documents can be handed to `crossplane
// render` exactly as helm produced them.
type Document struct {
	Object *unstructured.Unstructured
	Raw    []byte
	Class  Class
	// Source is the originating template path, from a `#### file:`/`# Source:`
	// marker or helm v4's postrender-filename annotation. Empty when unknown.
	Source string
}

// Stream is a classified input stream.
//
// XR and Composition are single-valued because `crossplane render` accepts
// exactly one of each; later documents overwrite earlier ones (last wins).
type Stream struct {
	XR                 *Document
	Composition        *Document
	Functions          []Document
	XRDs               []Document
	EnvironmentConfigs []Document
	Observed           []Document
	// Unknown documents are kept for diagnostics only; they are not rendered.
	Unknown []Document
}

// Marker prefixes that carry per-document provenance. helm-unittest injects
// `#### file:`; `helm template` emits `# Source:`.
const (
	markerUnittest = "#### file:"
	markerTemplate = "# Source:"
)

// filenameAnnotation is set by helm v4 on every document handed to a
// postrenderer/v1 plugin, and read back to regroup output into files.
const filenameAnnotation = "postrenderer.helm.sh/postrender-filename"

// markerPattern is built from the constants above so the literals are declared
// exactly once and cannot drift apart.
var markerPattern = regexp.MustCompile(
	`(?m)^(?:` + regexp.QuoteMeta(markerUnittest) + `|` + regexp.QuoteMeta(markerTemplate) + `)[[:space:]]*(.*)$`,
)

// Options configures classification.
type Options struct {
	// APIGroupDomain identifies XRs: a document whose apiVersion contains this
	// substring is the composite resource to render. Required.
	APIGroupDomain string
}

// Parse reads YAML documents from r and classifies each one.
//
// Documents that fail to parse are not fatal: they are collected as Unknown, so
// a single malformed document cannot fail an entire render the way an
// unguarded parse error would. Callers decide whether a missing XR or
// Composition is an error -- see Stream.Validate.
func Parse(r io.Reader, o Options) (*Stream, error) {
	if o.APIGroupDomain == "" {
		return nil, errors.New("cannot classify documents: no API group domain configured")
	}

	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, errors.Wrap(err, "cannot read input stream")
	}

	docs, err := splitDocuments(raw)
	if err != nil {
		return nil, err
	}

	s := &Stream{}
	for _, d := range docs {
		s.add(classify(d, o))
	}

	return s, nil
}

// Validate reports whether the stream has the inputs `crossplane render`
// requires. The messages name the missing input explicitly.
func (s *Stream) Validate(apiGroupDomain string) error {
	if s.XR == nil {
		return errors.Errorf("no XR found with API group domain %q in input", apiGroupDomain)
	}
	if s.Composition == nil {
		return errors.New("no Composition found in input")
	}
	if len(s.Functions) == 0 {
		return errors.New("no Functions found in input")
	}
	return nil
}

// add files a classified document into its bucket.
func (s *Stream) add(d Document) {
	switch d.Class {
	case ClassXR:
		s.XR = &d
	case ClassComposition:
		s.Composition = &d
	case ClassFunction:
		s.Functions = append(s.Functions, d)
	case ClassXRD:
		s.XRDs = append(s.XRDs, d)
	case ClassEnvironmentConfig, ClassExtraResource:
		// Extra-resources share the --extra-resources bucket with
		// EnvironmentConfigs; render resolves go-templating ExtraResources
		// requirements against both.
		s.EnvironmentConfigs = append(s.EnvironmentConfigs, d)
	case ClassObserved:
		s.Observed = append(s.Observed, d)
	case ClassUnknown:
		s.Unknown = append(s.Unknown, d)
	}
}

// rawDoc is an undecoded document plus any marker source preceding it.
type rawDoc struct {
	bytes  []byte
	source string
}

// splitDocuments splits a multi-document YAML stream.
//
// It uses apimachinery's YAML reader rather than splitting on "---" by hand, so
// separators inside block scalars and quoted strings do not split a document --
// a line-oriented scan would split here incorrectly.
func splitDocuments(raw []byte) ([]rawDoc, error) {
	reader := k8syaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))

	var out []rawDoc
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.Wrap(err, "cannot split input into YAML documents")
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		if isCommentOnly(doc) {
			continue
		}
		out = append(out, rawDoc{bytes: doc, source: markerSource(doc)})
	}

	return out, nil
}

// isCommentOnly reports whether a document has no content besides blank lines,
// comments, and document separators. These are skipped explicitly, since a
// marker line alone produces one.
//
// The separator check matters because apimachinery's YAML reader retains the
// leading "---" in each document it returns, so a comment-only document arrives
// as "---\n# comment\n" rather than just "# comment\n".
func isCommentOnly(doc []byte) bool {
	for line := range strings.SplitSeq(string(doc), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || t == "---" || strings.HasPrefix(t, "#") {
			continue
		}
		return false
	}
	return true
}

// markerSource extracts a template path from a `#### file:`/`# Source:` marker.
func markerSource(doc []byte) string {
	m := markerPattern.FindSubmatch(doc)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(string(m[1]))
}

// classify decodes a document and determines which render input it feeds.
//
// Check order matters: the test-injection annotations are honoured before
// kind/apiVersion routing, so an annotated XR lands in the
// observed/extra-resources bucket rather than overwriting the XR being
// rendered.
func classify(d rawDoc, o Options) Document {
	out := Document{Raw: d.bytes, Source: d.source, Class: ClassUnknown}

	obj := &unstructured.Unstructured{}
	if err := k8syaml.Unmarshal(d.bytes, obj); err != nil || obj.Object == nil {
		// Unparseable or empty: leave as Unknown rather than failing the render.
		return out
	}
	out.Object = obj

	if out.Source == "" {
		if v, ok := obj.GetAnnotations()[filenameAnnotation]; ok {
			out.Source = v
		}
	}

	if isTruthyAnnotation(obj, annotationTestObserved) {
		out.Class = ClassObserved
		return out
	}
	if isTruthyAnnotation(obj, annotationTestExtraResource) {
		out.Class = ClassExtraResource
		return out
	}

	switch obj.GetKind() {
	case kindComposition:
		out.Class = ClassComposition
		return out
	case kindFunction:
		out.Class = ClassFunction
		return out
	case kindXRD:
		out.Class = ClassXRD
		return out
	case kindEnvConfig:
		out.Class = ClassEnvironmentConfig
		return out
	}

	// An XR is identified by its API group, not its kind -- the kind is
	// composition-specific and unknown to us.
	if strings.Contains(obj.GetAPIVersion(), o.APIGroupDomain) {
		out.Class = ClassXR
	}

	return out
}

// isTruthyAnnotation reports whether an annotation is present and set to
// "true". Parsing the value means a value of "false" is correctly not
// truthy, and an unquoted true, which YAML permits, is also accepted -- a
// text search for the literal `: "true"` would miss that case.
func isTruthyAnnotation(obj *unstructured.Unstructured, key string) bool {
	v, ok := obj.GetAnnotations()[key]
	if !ok {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(v), "true")
}
