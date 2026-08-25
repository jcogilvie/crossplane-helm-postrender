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

// Package version reports the build version of the binary.
package version

import "runtime/debug"

// version is set at build time via
//
//	-ldflags "-X github.com/jcogilvie/crossplane-helm-postrender/internal/version.version=v1.2.3"
//
// Release builds set it explicitly. Builds that do not (a plain `go build`, or
// `go install`) fall back to the module version the toolchain records, so a
// binary can still identify itself.
var version string

// Get returns the build version, or "devel" when it cannot be determined.
func Get() string {
	if version != "" {
		return version
	}

	// `go install module@version` stamps the module version here even without
	// ldflags. A locally-built binary reports "(devel)", which is not useful to
	// print verbatim.
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}

	return "devel"
}
