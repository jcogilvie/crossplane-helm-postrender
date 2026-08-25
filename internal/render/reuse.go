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
	"os/exec"
	"regexp"
	"strings"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"

	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"

	"github.com/crossplane/cli/v2/cmd/crossplane/render"
)

// DefaultReuseSuffix is appended to derived container names when reuse is enabled
// without an explicit suffix.
//
// Fixed rather than derived from the working directory, so two projects on one
// machine share containers. Function containers are stateless gRPC servers keyed
// by image, so sharing is a feature -- the second project starts instantly.
// Callers wanting isolation pass their own suffix.
const DefaultReuseSuffix = "render"

// Annotations the crossplane CLI reads to decide a Function container's name and
// what happens to it after a render.
//
// Re-declared rather than referenced from the CLI because only
// AnnotationKeyRuntimeDockerCleanup is exported there;
// AnnotationKeyRuntimeNamedContainer is exported too, but taking both from one
// place keeps the pair visibly related.
const (
	annotationContainerName = "render.crossplane.io/runtime-docker-name"
	annotationCleanup       = "render.crossplane.io/runtime-docker-cleanup"

	// cleanupOrphan leaves the container running when the render finishes, which
	// is what makes it available to reuse.
	cleanupOrphan = "Orphan"
)

// nonDNSChars matches everything Docker will not accept in a container name.
// Docker allows [a-zA-Z0-9][a-zA-Z0-9_.-]*; package versions routinely contain
// characters outside that set.
var nonDNSChars = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

// validNetworkName is what Docker accepts for a network name. Checked before the
// name reaches a subprocess.
var validNetworkName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// ensureNetwork creates the Docker network if it does not already exist.
//
// Reuse needs a network that outlives the render, so the engine's own
// create-and-delete behaviour cannot be used -- but the engine also will not
// create a network it was told to join. Rather than make that the caller's setup
// step (and a confusing failure when they forget: "network ... not found" from
// deep inside container startup), create it here.
//
// Shells out to `docker` rather than using the Docker SDK: this is one idempotent
// command, the SDK is a heavy dependency for it, and the CLI has to be present
// anyway for rendering to work at all.
func ensureNetwork(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}

	// Validated before use. Arguments go to exec as a list, so there is no shell to
	// inject into, but a name Docker would reject produces a confusing failure and
	// an unvalidated one has no business reaching a subprocess at all.
	if !validNetworkName.MatchString(name) {
		return errors.Errorf("invalid Docker network name %q: expected %s", name, validNetworkName)
	}

	// Already there is the common case; asking first keeps the log quiet.
	if err := exec.CommandContext(ctx, "docker", "network", "inspect", name).Run(); err == nil { //nolint:gosec // name validated above; args passed as a list, not a shell string
		return nil
	}

	out, err := exec.CommandContext(ctx, "docker", "network", "create", name).CombinedOutput() //nolint:gosec // name validated above; args passed as a list, not a shell string
	if err == nil {
		return nil
	}

	// A concurrent render may have created it between the inspect and the create --
	// two suites running in parallel is exactly the case reuse is for. Treat an
	// existing network as success rather than a race we lost.
	if strings.Contains(string(out), "already exists") {
		return nil
	}

	return errors.Wrapf(err, "cannot create Docker network %q for container reuse: %s",
		name, strings.TrimSpace(string(out)))
}

// enableContainerReuse annotates fns so their containers survive the render and
// are found again by the next one.
//
// Starting Function containers dominates render cost, and a post-renderer is
// spawned once per unit test, so without reuse a suite pays that cost per test.
// The crossplane CLI supports reuse through two annotations -- a stable container
// name, and Orphan cleanup so the container is left running -- but expects them to
// be written by hand on every Function.
//
// That is the wrong place for them. Container reuse is this tool's optimisation,
// not a property of anyone's chart, and the names are mechanical enough to derive.
// Requiring users to annotate every Function in every chart to get it, and to keep
// those annotations correct, makes a local performance concern part of their API.
// So the annotations are injected here instead, from one flag.
//
// A Function that already carries either annotation is left alone, so an explicit
// choice in a chart still wins over the derived one.
//
// Note the network annotation is deliberately NOT set here: the CLI's own
// injectNetworkAnnotation already fills it in from whichever network the engine
// joins, so setting it too would only create a second value to keep in sync.
func enableContainerReuse(fns []pkgv1.Function, suffix string) error {
	if suffix == "" {
		suffix = DefaultReuseSuffix
	}

	for i := range fns {
		existing := fns[i].GetAnnotations()

		// All-or-nothing per Function. Injecting a name onto a Function that
		// explicitly asked for Remove cleanup, or Orphan onto one with a
		// hand-picked name, would produce a combination the chart never asked for.
		if _, ok := existing[annotationContainerName]; ok {
			continue
		}
		if _, ok := existing[annotationCleanup]; ok {
			continue
		}

		// Reuse the CLI's own annotation applier rather than writing the map by
		// hand, so the key=value parsing and nil-map handling stay in one place.
		// Note it must be given the slice, not a copy of the element: a copy shares
		// nothing once SetAnnotations replaces the map, so the mutation would be
		// lost. Verified, not assumed.
		if err := render.OverrideFunctionAnnotations(fns[i:i+1], []string{
			annotationContainerName + "=" + containerName(&fns[i], suffix),
			annotationCleanup + "=" + cleanupOrphan,
		}); err != nil {
			return err
		}
	}

	return nil
}

// containerName derives a stable Docker container name for a Function.
//
// Two Functions must never collide, and the same Function must produce the same
// name on every run or nothing is ever reused. The package reference is included
// because it carries the version: upgrading a Function should start a fresh
// container rather than silently reuse one running the old image.
func containerName(fn *pkgv1.Function, suffix string) string {
	parts := []string{sanitize(fn.GetName())}

	// Package references look like
	// xpkg.crossplane.io/crossplane-contrib/function-go-templating:v0.12.2, or
	// .../function-x@sha256:abc123 for a digest. The tag or digest distinguishes
	// versions; the registry path is already implied by the Function's name.
	//
	// Cut on the *first* separator of the final path segment, so a digest keeps its
	// algorithm: cutting on ":" alone turns "f@sha256:abc" into "abc", and two
	// digests differing only in algorithm would then collide.
	if pkg := fn.Spec.Package; pkg != "" {
		if ref := packageVersion(lastPathSegment(pkg)); ref != "" {
			parts = append(parts, sanitize(ref))
		}
	}

	parts = append(parts, sanitize(suffix))

	// Docker requires the first character to be alphanumeric.
	name := strings.Join(parts, "-")
	return strings.TrimLeft(name, "_.-")
}

// lastPathSegment returns everything after the final "/", so a registry path does
// not contribute separators to the container name.
func lastPathSegment(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// packageVersion returns the version part of a package reference's final path
// segment -- the tag after ":", or the whole digest after "@" including its
// algorithm.
//
// Cutting at the first separator rather than at ":" specifically is what keeps
// "sha256" in a digest: "f@sha256:abc" must yield "sha256:abc", not "abc", or two
// digests differing only in algorithm would derive the same container name.
// Returns "" when the reference carries no version.
func packageVersion(segment string) string {
	if i := strings.IndexAny(segment, ":@"); i >= 0 {
		return segment[i+1:]
	}
	return ""
}

// sanitize replaces characters Docker rejects in a container name with "-", and
// collapses runs of them so a digest reference does not produce "---".
func sanitize(s string) string {
	return strings.Trim(nonDNSChars.ReplaceAllString(s, "-"), "-")
}
