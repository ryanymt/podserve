// Command podserve deploys Hugging Face models on GKE behind an OpenAI-compatible API.
// docs/design/cli.md is the contract for every command, flag, output line and exit code.
//
// main.go fixes the seam tests use (P01 T1): run takes every outside dependency through deps, so
// testdata/spine/cli/cli_test.go drives the real commands against a fake Hub, internal/fakekube and
// a fake clock, and captures what would be exec'd (gcloud, kubectl). T10b implements the commands
// in other files of this package; it may extend deps but not change the fields below.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"k8s.io/client-go/dynamic"

	"github.com/podserve/podserve/pkg/profile"
)

// HFClient is what the CLI needs from pkg/hf (satisfied by *hf.Client).
type HFClient interface {
	Model(ctx context.Context, repo, revision string) (profile.Model, error)
}

// deps are run's outside world.
type deps struct {
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	// NewHF returns a Hub client; token is "" for anonymous access.
	NewHF func(token string) HFClient
	// NewDynamic returns a client for the current kubeconfig context.
	NewDynamic func() (dynamic.Interface, error)
	// Exec runs an external command (gcloud, kubectl) with the given stdio and returns its error.
	Exec func(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error
	// Clock for waits and timing.
	Now   func() time.Time
	Sleep func(time.Duration)
	// Version is printed by `podserve version`.
	Version string
}

// version is set at release build time with -ldflags "-X main.version=v0.1.0".
var version = "dev"

func main() {
	os.Exit(run(context.Background(), os.Args[1:], defaultDeps()))
}

// run executes one command line and returns the process exit code (docs/design/cli.md). The
// commands map their own errors through *exitError (root.go); anything else cobra produced itself
// (unknown flag, missing required flag, unknown command) is usage and exits 2.
func run(ctx context.Context, args []string, d deps) int {
	root := newRoot(d)
	root.SetArgs(args)
	root.SetOut(d.Stdout)
	root.SetErr(d.Stderr)
	root.SilenceUsage = true
	root.SilenceErrors = true
	err := root.ExecuteContext(ctx)
	if err == nil {
		return exitOK
	}
	var e *exitError
	if errors.As(err, &e) {
		for _, line := range e.lines {
			fmt.Fprintln(d.Stderr, "podserve: "+line)
		}
		return e.code
	}
	fmt.Fprintln(d.Stderr, "podserve: "+err.Error())
	return exitUsage
}
