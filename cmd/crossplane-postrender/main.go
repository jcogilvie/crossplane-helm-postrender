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

// Command crossplane-postrender is a Helm post-renderer that renders Crossplane
// compositions.
//
// It reads a rendered manifest stream on stdin and writes the composed resources
// on stdout -- the contract Helm defines for a post-renderer. Charts can
// therefore be unit-tested against the managed resources their compositions
// actually produce, rather than against the XRs they declare.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/alecthomas/kong"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	"github.com/jcogilvie/crossplane-helm-postrender/internal/parse"
	"github.com/jcogilvie/crossplane-helm-postrender/internal/render"
	"github.com/jcogilvie/crossplane-helm-postrender/internal/version"
)

// cli is the command line interface.
//
// The API group domain is a positional argument so it can be supplied through
// Helm's --post-renderer-args, which appends values after the plugin command.
//
// It is required rather than defaulted: the domain is what distinguishes the XR
// to render from every other document in the stream, and guessing it wrong
// produces "no XR found" rather than anything useful. An explicit value is also
// self-documenting at the call site.
type cli struct {
	Version kong.VersionFlag `help:"Print the version and exit." short:"V"`

	APIGroupDomain string `arg:"" help:"API group domain identifying XRs to render, e.g. platform.example.org." name:"api-group-domain"`

	CrossplaneVersion string `env:"CROSSPLANE_RENDER_VERSION" help:"Pin the core-crossplane render-engine image." name:"crossplane-version"`
	DockerNetwork     string `env:"CROSSPLANE_DOCKER_NETWORK" help:"Docker network the render engine joins."      name:"docker-network"`
	Verbose           bool   `env:"CROSSPLANE_RENDER_VERBOSE" help:"Log render diagnostics to stderr."            short:"v"`

	// Stdin and Stdout are the streams Run reads and writes. They are fields,
	// not hardcoded os.Stdin/os.Stdout, so Run is testable; both default when
	// nil. kong:"-" keeps them off the command line.
	Stdin  io.Reader `kong:"-"`
	Stdout io.Writer `kong:"-"`
}

func main() {
	c := &cli{}
	ctx := kong.Parse(c,
		kong.Name("crossplane-postrender"),
		kong.Description("Helm post-renderer that renders Crossplane compositions."),
		kong.UsageOnError(),
		kong.Vars{"version": version.Get()},
	)

	ctx.FatalIfErrorf(c.Run())
}

// Run reads stdin, renders, and writes the result to stdout.
func (c *cli) Run() error {
	stdin, stdout := io.Reader(os.Stdin), io.Writer(os.Stdout)
	if c.Stdin != nil {
		stdin = c.Stdin
	}
	if c.Stdout != nil {
		stdout = c.Stdout
	}

	// A no-op logger by default. The render engine is chatty, and anything it
	// writes to stdout would corrupt the manifest stream Helm parses.
	log := logging.NewNopLogger()
	if c.Verbose {
		log = logging.NewLogrLogger(zap.New(zap.UseDevMode(true)))
	}

	stream, err := parse.Parse(stdin, parse.Options{APIGroupDomain: c.APIGroupDomain})
	if err != nil {
		return err
	}

	r := render.NewRenderer(render.Options{
		CrossplaneVersion: c.CrossplaneVersion,
		DockerNetwork:     c.DockerNetwork,
		Logger:            log,
	})

	res, err := r.Render(context.Background(), stream, c.APIGroupDomain)
	if err != nil {
		return err
	}

	out, err := res.YAML()
	if err != nil {
		return err
	}

	if _, err := stdout.Write(out); err != nil {
		return fmt.Errorf("cannot write rendered manifests: %w", err)
	}

	return nil
}
