package hf_test

// Spine test (P01 T2), parked under testdata/spine/hf/. T5 moves it to pkg/hf/ with `git mv`,
// unchanged. An httptest server plays the Hugging Face Hub from the recordings in testdata/hf/
// (hack/record-hf-fixture.py); every model hf.Client returns must equal what internal/hffixture
// (an independent implementation of the same contract) derives from the same files.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ryanymt/podserve/internal/hffixture"
	"github.com/ryanymt/podserve/pkg/hf"
	"github.com/ryanymt/podserve/pkg/platform"
)

type fakeHub struct {
	mu       sync.Mutex
	fixtures map[string]*hffixture.Fixture // by repo
	requests []string                      // "AUTH METHOD PATH" ("-" when no Authorization header)
	failInfo int                           // when non-zero, the model-info call returns this status
	t        *testing.T
}

func newFakeHub(t *testing.T) (*fakeHub, *httptest.Server) {
	h := &fakeHub{fixtures: map[string]*hffixture.Fixture{}, t: t}
	repos, err := hffixture.Repos()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range repos {
		f, err := hffixture.Load(r)
		if err != nil {
			t.Fatal(err)
		}
		h.fixtures[r] = f
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, srv
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		auth = "-"
	}
	h.mu.Lock()
	h.requests = append(h.requests, auth+" "+r.Method+" "+r.URL.Path)
	h.mu.Unlock()

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	// /api/models/ORG/MODEL/revision/REV
	case len(parts) == 6 && parts[0] == "api" && parts[1] == "models" && parts[4] == "revision":
		if h.failInfo != 0 {
			w.WriteHeader(h.failInfo)
			return
		}
		f, ok := h.fixtures[parts[2]+"/"+parts[3]]
		if !ok || (parts[5] != "main" && parts[5] != f.Info.SHA) {
			http.Error(w, `{"error":"Repository not found"}`, http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("blobs") != "true" {
			h.t.Errorf("model-info request without blobs=true: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(f.InfoJSON)
	// /ORG/MODEL/resolve/SHA/FILE
	case len(parts) == 5 && parts[2] == "resolve":
		f, ok := h.fixtures[parts[0]+"/"+parts[1]]
		if !ok || parts[3] != f.Info.SHA {
			http.NotFound(w, r)
			return
		}
		switch parts[4] {
		case "config.json":
			if f.ConfigJSON == nil {
				http.Error(w, "Access to model is restricted", http.StatusUnauthorized)
				return
			}
			w.Write(f.ConfigJSON)
		case "model.safetensors.index.json":
			if f.IndexJSON == nil {
				http.NotFound(w, r)
				return
			}
			w.Write(f.IndexJSON)
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func (h *fakeHub) reset() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.requests
	h.requests = nil
	return r
}

func client(srv *httptest.Server, token string) *hf.Client {
	return &hf.Client{BaseURL: srv.URL, HTTP: srv.Client(), Token: token}
}

func TestEveryFixtureMatchesTheReference(t *testing.T) {
	hub, srv := newFakeHub(t)
	c := client(srv, "")
	for repo, f := range hub.fixtures {
		t.Run(repo, func(t *testing.T) {
			hub.reset()
			got, err := c.Model(context.Background(), repo, "main")
			if err != nil {
				t.Fatal(err)
			}
			want, err := f.Model(false)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("model mismatch\n got: %+v\nwant: %+v", got, want)
				if got.Config != nil && want.Config != nil {
					t.Errorf("config\n got: %+v\nwant: %+v", *got.Config, *want.Config)
				}
			}
			reqs := hub.reset()
			for _, r := range reqs {
				if strings.HasSuffix(r, "/config.json") && want.Gated {
					t.Errorf("config.json requested for a gated model without a token: %s", r)
				}
				if !strings.HasPrefix(r, "- ") {
					t.Errorf("sent an Authorization header with no token: %s", r)
				}
			}
		})
	}
}

func TestRevisionBySHA(t *testing.T) {
	hub, srv := newFakeHub(t)
	sha := hub.fixtures["Qwen/Qwen3-8B"].Info.SHA
	got, err := client(srv, "").Model(context.Background(), "Qwen/Qwen3-8B", sha)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != sha || got.Config == nil {
		t.Errorf("revision %q config %v", got.Revision, got.Config)
	}
}

func TestTokenIsSentOnEveryRequest(t *testing.T) {
	hub, srv := newFakeHub(t)
	if _, err := client(srv, "hf_test").Model(context.Background(), "Qwen/Qwen3-8B", "main"); err != nil {
		t.Fatal(err)
	}
	reqs := hub.reset()
	if len(reqs) < 3 { // info, config, index
		t.Fatalf("want at least 3 requests, got %v", reqs)
	}
	for _, r := range reqs {
		if !strings.HasPrefix(r, "Bearer hf_test ") {
			t.Errorf("request without the token: %s", r)
		}
	}
}

func TestGatedWithTokenButRefused(t *testing.T) {
	_, srv := newFakeHub(t)
	_, err := client(srv, "hf_test").Model(context.Background(), "meta-llama/Llama-3.1-8B-Instruct", "main")
	if !errors.Is(err, hf.ErrAccessDenied) {
		t.Fatalf("want ErrAccessDenied, got %v", err)
	}
}

func TestNotFound(t *testing.T) {
	_, srv := newFakeHub(t)
	for _, tc := range [][2]string{{"nobody/nothing", "main"}, {"Qwen/Qwen3-8B", "no-such-branch"}} {
		_, err := client(srv, "").Model(context.Background(), tc[0], tc[1])
		if !errors.Is(err, hf.ErrNotFound) {
			t.Errorf("%s@%s: want ErrNotFound, got %v", tc[0], tc[1], err)
		}
	}
}

func TestOtherStatusIsAnError(t *testing.T) {
	hub, srv := newFakeHub(t)
	hub.failInfo = http.StatusInternalServerError
	_, err := client(srv, "").Model(context.Background(), "Qwen/Qwen3-8B", "main")
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "/api/models/Qwen/Qwen3-8B") {
		t.Fatalf("want an error naming status 500 and the URL, got %v", err)
	}
	if errors.Is(err, hf.ErrNotFound) || errors.Is(err, hf.ErrAccessDenied) {
		t.Errorf("a 500 must not look like not-found or access-denied: %v", err)
	}
}

func TestNewUsesThePublicHub(t *testing.T) {
	c := hf.New("tok")
	if c.BaseURL != hf.DefaultBaseURL || c.Token != "tok" || c.HTTP == nil {
		t.Errorf("New: %+v", c)
	}
}

func TestLargestFetchFile(t *testing.T) {
	for repo := range map[string]bool{"Qwen/Qwen3-8B": true, "JackFram/llama-160m": true, "mistralai/Ministral-8B-Instruct-2410": true} {
		f, err := hffixture.Load(repo)
		if err != nil {
			t.Fatal(err)
		}
		m, _ := f.Model(false)
		var want int64
		for _, file := range m.Files {
			if platform.FetchFileAllowed(file.Name) && file.Size > want {
				want = file.Size
			}
		}
		if got := hf.LargestFetchFile(m); got != want || got == 0 {
			t.Errorf("%s: LargestFetchFile = %d, want %d", repo, got, want)
		}
	}
}
