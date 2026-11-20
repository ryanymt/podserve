package kube_test

// Parked spine test (P02 T2) for T3: `git mv` it to pkg/kube/logs_test.go, unchanged.
// kube.NewLogReader (D-029) reads a container's previous run through the API server's pod log
// endpoint with only the pinned k8s.io/client-go/rest package.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/ryanymt/podserve/pkg/kube"
)

func TestLogReaderReadsThePreviousRun(t *testing.T) {
	var path, query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("usage: vllm [-h] [-v]\nvllm: error: unrecognized arguments: --no-such-flag\n"))
	}))
	defer srv.Close()
	lr, err := kube.NewLogReader(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	lines, err := lr.PreviousLog(context.Background(), "qwen3-8b-745f4d9766-rtxmk", "vllm", 20)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"usage: vllm [-h] [-v]", "vllm: error: unrecognized arguments: --no-such-flag"}; !reflect.DeepEqual(lines, want) {
		t.Errorf("lines %q, want %q", lines, want)
	}
	if path != "/api/v1/namespaces/podserve/pods/qwen3-8b-745f4d9766-rtxmk/log" {
		t.Errorf("path %s", path)
	}
	for _, p := range []string{"container=vllm", "previous=true", "tailLines=20"} {
		if !contains(query, p) {
			t.Errorf("query %q lacks %s", query, p)
		}
	}
}

// The API server answers 400 when there is no previous run; that is an error, not empty lines.
func TestLogReaderReportsTheServersError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"previous terminated container \"vllm\" in pod \"p\" not found","reason":"BadRequest","code":400}`))
	}))
	defer srv.Close()
	lr, err := kube.NewLogReader(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if lines, err := lr.PreviousLog(context.Background(), "p", "vllm", 20); err == nil {
		t.Errorf("got lines %q and no error", lines)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
