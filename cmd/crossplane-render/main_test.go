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

package main

import (
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

// The CLI contract matters as much as the rendering: callers invoke this binary
// positionally through Helm's --post-renderer-args, and a silent change to how
// arguments are parsed would break them without any test failing.
func TestCLIParsesArgs(t *testing.T) {
	tests := map[string]struct {
		args        []string
		wantDomain  string
		wantVersion string
		wantNetwork string
		wantVerbose bool
		wantErr     bool
	}{
		// The domain is required: guessing it wrong yields "no XR found" rather
		// than anything useful, so it is better to refuse than to assume.
		"NoArgsIsAnError": {
			args:    []string{},
			wantErr: true,
		},
		"PositionalDomain": {
			args:       []string{"platform.example.org"},
			wantDomain: "platform.example.org",
		},
		"VersionFlag": {
			args:        []string{"platform.example.org", "--crossplane-version", "v2.4.0"},
			wantDomain:  "platform.example.org",
			wantVersion: "v2.4.0",
		},
		"NetworkFlag": {
			args:        []string{"platform.example.org", "--docker-network", "custom-net"},
			wantDomain:  "platform.example.org",
			wantNetwork: "custom-net",
		},
		"VerboseShorthand": {
			args:        []string{"platform.example.org", "-v"},
			wantDomain:  "platform.example.org",
			wantVerbose: true,
		},
		"UnknownFlagIsAnError": {
			args:    []string{"platform.example.org", "--no-such-flag"},
			wantErr: true,
		},
		// Only one positional is defined; a second must not be silently ignored.
		"TooManyPositionals": {
			args:    []string{"one.example.com", "two.example.com"},
			wantErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var c cli
			parser, err := kong.New(&c, kong.Name("crossplane-render"), kong.Exit(func(int) {}))
			if err != nil {
				t.Fatalf("kong.New(...): unexpected error: %v", err)
			}

			_, err = parser.Parse(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Errorf("Parse(%v): want an error, got nil", tt.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%v): unexpected error: %v", tt.args, err)
			}

			if c.APIGroupDomain != tt.wantDomain {
				t.Errorf("APIGroupDomain = %q, want %q", c.APIGroupDomain, tt.wantDomain)
			}
			if c.CrossplaneVersion != tt.wantVersion {
				t.Errorf("CrossplaneVersion = %q, want %q", c.CrossplaneVersion, tt.wantVersion)
			}
			if c.DockerNetwork != tt.wantNetwork {
				t.Errorf("DockerNetwork = %q, want %q", c.DockerNetwork, tt.wantNetwork)
			}
			if c.Verbose != tt.wantVerbose {
				t.Errorf("Verbose = %v, want %v", c.Verbose, tt.wantVerbose)
			}
		})
	}
}

// Env vars are a supported way to configure this binary, so they are part of the
// contract too.
func TestCLIReadsEnv(t *testing.T) {
	t.Setenv("CROSSPLANE_RENDER_VERSION", "v9.9.9")
	t.Setenv("CROSSPLANE_DOCKER_NETWORK", "env-net")
	t.Setenv("CROSSPLANE_RENDER_VERBOSE", "true")

	var c cli
	parser, err := kong.New(&c, kong.Name("crossplane-render"), kong.Exit(func(int) {}))
	if err != nil {
		t.Fatalf("kong.New(...): unexpected error: %v", err)
	}
	if _, err := parser.Parse([]string{"platform.example.org"}); err != nil {
		t.Fatalf("Parse(...): unexpected error: %v", err)
	}

	if c.CrossplaneVersion != "v9.9.9" {
		t.Errorf("CrossplaneVersion = %q, want it read from the environment", c.CrossplaneVersion)
	}
	if c.DockerNetwork != "env-net" {
		t.Errorf("DockerNetwork = %q, want it read from the environment", c.DockerNetwork)
	}
	if !c.Verbose {
		t.Error("Verbose = false, want it read from the environment")
	}
}

// A flag must win over the environment, or a caller cannot override what the
// surrounding shell has already set.
func TestCLIFlagBeatsEnv(t *testing.T) {
	t.Setenv("CROSSPLANE_DOCKER_NETWORK", "env-net")

	var c cli
	parser, err := kong.New(&c, kong.Name("crossplane-render"), kong.Exit(func(int) {}))
	if err != nil {
		t.Fatalf("kong.New(...): unexpected error: %v", err)
	}
	if _, err := parser.Parse([]string{"platform.example.org", "--docker-network", "flag-net"}); err != nil {
		t.Fatalf("Parse(...): unexpected error: %v", err)
	}

	if c.DockerNetwork != "flag-net" {
		t.Errorf("DockerNetwork = %q, want the flag to win over the environment", c.DockerNetwork)
	}
}

// Empty stdin must produce a clear diagnostic. Helm surfaces only "plugin exited
// with error", so an unhelpful message here is genuinely hard to debug.
func TestRunRejectsEmptyStdin(t *testing.T) {
	c := &cli{APIGroupDomain: "platform.example.org", Stdin: strings.NewReader("")}

	err := c.Run()
	if err == nil {
		t.Fatal("Run(): want an error for empty stdin, got nil")
	}
	if !strings.Contains(err.Error(), "no XR found") {
		t.Errorf("Run(): want a no-XR error naming the problem, got %q", err)
	}
}

// A stream with no XR of the configured API group must say so explicitly. The
// most common cause is a mistyped domain, and naming it in the message is what
// makes that diagnosable.
func TestRunReportsMissingXR(t *testing.T) {
	const noXR = `apiVersion: v1
kind: ConfigMap
metadata:
  name: unrelated
`
	c := &cli{APIGroupDomain: "platform.example.org", Stdin: strings.NewReader(noXR)}

	err := c.Run()
	if err == nil {
		t.Fatal("Run(): want an error when no XR is present, got nil")
	}
	if !strings.Contains(err.Error(), "no XR found with API group domain") {
		t.Errorf("Run(): want an error naming the API group domain, got %q", err)
	}
}
