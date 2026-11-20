package main

// Parked spine test (P02 T2) for T7: `git mv` it to cmd/podserve/init_stderr_test.go, unchanged.
// `podserve init` sends gcloud's build log to stderr, so stdout carries only init's own lines
// (docs/design/cli.md, `podserve init`).

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestInitBuildLogGoesToStderr(t *testing.T) {
	initDir(t, true)
	e := newEnv(t, true)
	d := e.deps()
	inner := d.Exec
	d.Exec = func(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
		if name == "gcloud" && len(args) > 1 && args[0] == "builds" && args[1] == "submit" {
			fmt.Fprintln(stdout, "BUILD LOG: Step 1/6 : FROM python")
			fmt.Fprintln(stderr, "BUILD LOG: Uploading tarball")
		}
		return inner(ctx, stdout, stderr, name, args...)
	}
	if code := run(context.Background(), []string{"init", "--config", "terraform-output.json"}, d); code != 0 {
		t.Fatalf("init exit %d, stderr: %s", code, e.stderr.String())
	}
	if strings.Contains(e.stdout.String(), "BUILD LOG") {
		t.Errorf("stdout carries the build log:\n%s", e.stdout.String())
	}
	for _, line := range []string{"BUILD LOG: Step 1/6 : FROM python", "BUILD LOG: Uploading tarball"} {
		if !strings.Contains(e.stderr.String(), line) {
			t.Errorf("stderr lacks %q:\n%s", line, e.stderr.String())
		}
	}
	if !strings.HasPrefix(e.stdout.String(), "Building the fetch image with Cloud Build...\nFetch image: ") {
		t.Errorf("stdout should still start with init's own lines:\n%s", e.stdout.String())
	}
}
