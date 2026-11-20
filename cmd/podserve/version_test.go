package main

import (
	"context"
	"runtime/debug"
	"slices"
	"testing"
)

// The shapes Go 1.27 records (checked in P02's red team): a tag from `go install …@vX.Y.Z` or a
// tagged checkout, a pseudo-version from `go build` in an untagged checkout, "+dirty" for a modified
// tree, and "(devel)" with no VCS settings from `go test` and `go run`.
func TestVersionFrom(t *testing.T) {
	for _, c := range []struct {
		name string
		info *debug.BuildInfo
		ok   bool
		want string
	}{
		{"go install at a tag", &debug.BuildInfo{Main: debug.Module{Version: "v0.1.1"}}, true, "v0.1.1"},
		{"untagged checkout", &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261119024823-322d7cb47438"}}, true,
			"v0.0.0-20261119024823-322d7cb47438"},
		{"dirty tree after a tag", &debug.BuildInfo{Main: debug.Module{Version: "v0.1.2-0.20261119030000-debedd6564cc+dirty"}}, true,
			"v0.1.2-0.20261119030000-debedd6564cc+dirty"},
		{"go run or go test", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true, "dev"},
		{"empty version", &debug.BuildInfo{}, true, "dev"},
		{"no build info", nil, false, "dev"},
	} {
		if got := versionFrom(c.info, c.ok); got != c.want {
			t.Errorf("%s: versionFrom = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestImageTag(t *testing.T) {
	for in, want := range map[string]string{
		"v0.1.1": "v0.1.1",
		"dev":    "dev",
		"v0.1.2-0.20261119030000-debedd6564cc+dirty": "v0.1.2-0.20261119030000-debedd6564cc-dirty",
	} {
		if got := imageTag(in); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", in, got, want)
		}
	}
}

// init passes a Docker-valid tag to Cloud Build even for a "+dirty" build.
func TestInitTagsADirtyBuild(t *testing.T) {
	initDir(t, true)
	e := newEnv(t, true)
	d := e.deps()
	d.Version = "v0.1.2-0.20261119030000-debedd6564cc+dirty"
	if code := run(context.Background(), []string{"init", "--config", "terraform-output.json"}, d); code != 0 {
		t.Fatalf("init exit %d, stderr: %s", code, e.stderr.String())
	}
	want := "us-central1-docker.pkg.dev/my-project/podserve/fetch:v0.1.2-0.20261119030000-debedd6564cc-dirty"
	if len(e.execs) == 0 || !slices.Contains(e.execs[0], want) {
		t.Errorf("gcloud builds submit args %v, want --tag %s", e.execs, want)
	}
}
