package hf_test

// Parked spine test (P02 T2) for T6: `git mv` it to pkg/hf/revision_test.go, unchanged.
// A revision is one path segment in the Hub's API, so one that contains "/" (a pull-request ref
// such as refs/pr/1, or a branch like release/v2) must be path-escaped. The repo id keeps its "/".

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ryanymt/podserve/pkg/hf"
)

func TestRevisionIsPathEscaped(t *testing.T) {
	for revision, want := range map[string]string{
		"main":       "/api/models/Qwen/Qwen3-8B/revision/main?blobs=true",
		"refs/pr/1":  "/api/models/Qwen/Qwen3-8B/revision/refs%2Fpr%2F1?blobs=true",
		"release/v2": "/api/models/Qwen/Qwen3-8B/revision/release%2Fv2?blobs=true",
	} {
		var got []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = append(got, r.RequestURI)
			http.NotFound(w, r)
		}))
		c := &hf.Client{BaseURL: srv.URL, HTTP: srv.Client()}
		_, _ = c.Model(context.Background(), "Qwen/Qwen3-8B", revision)
		srv.Close()
		if len(got) == 0 || got[0] != want {
			t.Errorf("revision %q: requested %v, want first request %s", revision, got, want)
		}
	}
}
