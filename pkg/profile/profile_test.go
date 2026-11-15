package profile_test

// Spine test (P01 T2), parked under testdata/spine/profile/. T3 moves it to pkg/profile/ with
// `git mv`, unchanged. It runs every case in testdata/profile/cases.json through profile.Plan and
// compares the result, field by field, with testdata/profile/expected.json, which
// testdata/spine/profile/expected.py generates from docs/design/profile.md independently of the Go
// code (tests/profile-expected.test.sh keeps it fresh).

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/podserve/podserve/catalog"
	"github.com/podserve/podserve/internal/hffixture"
	"github.com/podserve/podserve/pkg/profile"
)

type caseFile struct {
	Cases []struct {
		Name    string          `json:"name"`
		Repo    string          `json:"repo"`
		Request profile.Request `json:"request"`
	} `json:"cases"`
}

type expectedFile struct {
	Cases []struct {
		Name    string          `json:"name"`
		Result  json.RawMessage `json:"result"`
		Refusal *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"refusal"`
	} `json:"cases"`
}

func readJSON(t *testing.T, rel string, v any) {
	t.Helper()
	root, err := hffixture.Root() // .../testdata/hf
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(root), rel))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
}

// normalise turns a Result (or expected result) into a generic map with "warnings" always a list.
func normalise(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["warnings"] == nil {
		m["warnings"] = []any{}
	}
	return m
}

func TestPlanMatchesReference(t *testing.T) {
	cat, err := profile.LoadCatalog(catalog.GPUsYAML)
	if err != nil {
		t.Fatal(err)
	}
	archs := profile.LoadArchitectures(catalog.VLLMArchitectures)

	var cases caseFile
	var want expectedFile
	readJSON(t, "profile/cases.json", &cases)
	readJSON(t, "profile/expected.json", &want)
	if len(cases.Cases) != len(want.Cases) {
		t.Fatalf("cases.json has %d cases, expected.json %d: regenerate expected.json", len(cases.Cases), len(want.Cases))
	}

	for i, c := range cases.Cases {
		exp := want.Cases[i]
		t.Run(c.Name, func(t *testing.T) {
			if exp.Name != c.Name {
				t.Fatalf("expected.json case %d is %q, want %q", i, exp.Name, c.Name)
			}
			f, err := hffixture.Load(c.Repo)
			if err != nil {
				t.Fatal(err)
			}
			model, err := f.Model(c.Request.HasToken)
			if err != nil {
				t.Fatal(err)
			}
			got, err := profile.Plan(model, c.Request, cat, archs)

			if exp.Refusal != nil {
				var r *profile.Refusal
				if !errors.As(err, &r) {
					t.Fatalf("want refusal %s, got result %+v, err %v", exp.Refusal.Code, got, err)
				}
				if string(r.Code) != exp.Refusal.Code || r.Message != exp.Refusal.Message {
					t.Errorf("refusal\n got: %s %q\nwant: %s %q", r.Code, r.Message, exp.Refusal.Code, exp.Refusal.Message)
				}
				return
			}
			if err != nil {
				t.Fatalf("want a profile, got error %v", err)
			}
			gotJSON, _ := json.Marshal(got)
			g, w := normalise(t, gotJSON), normalise(t, exp.Result)
			for k, wv := range w {
				if !reflect.DeepEqual(g[k], wv) {
					t.Errorf("%s: got %v, want %v", k, g[k], wv)
				}
			}
			for k := range g {
				if _, ok := w[k]; !ok {
					t.Errorf("unexpected field %s in Result JSON", k)
				}
			}
		})
	}
}

func TestRefusalIsAnError(t *testing.T) {
	var err error = &profile.Refusal{Code: profile.DoesNotFit, Message: "x"}
	if err.Error() != "DOES_NOT_FIT: x" {
		t.Errorf("Refusal.Error() = %q", err.Error())
	}
}
