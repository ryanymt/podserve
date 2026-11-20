package hf

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/ryanymt/podserve/pkg/platform"
	"github.com/ryanymt/podserve/pkg/profile"
)

// Client talks to the Hub. The zero value is not usable; call New.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Token   string // "" for anonymous access
}

// New returns a Client for the public Hub. token may be "".
func New(token string) *Client {
	return &Client{BaseURL: DefaultBaseURL, HTTP: http.DefaultClient, Token: token}
}

// indexName is the sharded-weights manifest; its weight_map names the weight files.
const indexName = "model.safetensors.index.json"

// infoResponse is what Model reads from the model-info endpoint.
type infoResponse struct {
	ID          string `json:"id"`
	SHA         string `json:"sha"`
	Gated       any    `json:"gated"` // false, "auto" or "manual"
	PipelineTag string `json:"pipeline_tag"`
	CardData    struct {
		License string `json:"license"`
	} `json:"cardData"`
	Siblings []struct {
		RFilename string `json:"rfilename"`
		Size      int64  `json:"size"`
	} `json:"siblings"`
}

// Model reads one revision of repo ("ORG/MODEL"). revision is a branch, tag or SHA. It resolves the
// revision to its commit SHA and reads config.json and model.safetensors.index.json from that SHA.
// For a gated model without a token the Hub refuses every resolve call, so Config and
// IndexWeightFiles stay nil.
func (c *Client) Model(ctx context.Context, repo, revision string) (profile.Model, error) {
	info, err := c.modelInfo(ctx, repo, revision)
	if err != nil {
		return profile.Model{}, err
	}
	m := profile.Model{
		Repo:        info.ID,
		Revision:    info.SHA,
		PipelineTag: info.PipelineTag,
		Gated:       info.Gated == "auto" || info.Gated == "manual",
		License:     info.CardData.License,
	}
	for _, s := range info.Siblings {
		m.Files = append(m.Files, profile.File{Name: s.RFilename, Size: s.Size})
	}

	// A gated model answers 401 to every resolve made anonymously; the settings engine refuses
	// with GATED_NO_TOKEN, so there is nothing more to read.
	if m.Gated && c.Token == "" {
		return m, nil
	}
	if m.Config, err = c.fetchConfig(ctx, repo, info.SHA); err != nil {
		return profile.Model{}, err
	}
	for _, f := range m.Files { // only sharded revisions have an index
		if f.Name != indexName {
			continue
		}
		if m.IndexWeightFiles, err = c.fetchIndexWeightFiles(ctx, repo, info.SHA); err != nil {
			return profile.Model{}, err
		}
		break
	}
	return m, nil
}

// modelInfo resolves revision and returns the trimmed model-info response.
func (c *Client) modelInfo(ctx context.Context, repo, revision string) (*infoResponse, error) {
	// The revision is one path segment, so a "/" in it (refs/pr/1, release/v2) must be escaped; the
	// repo id keeps its own "/".
	path := "/api/models/" + repo + "/revision/" + url.PathEscape(revision) + "?blobs=true"
	status, body, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("GET %s: %w", path, ErrNotFound)
	}
	if status/100 != 2 {
		return nil, fmt.Errorf("GET %s: unexpected status %d", path, status)
	}
	var info infoResponse
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	return &info, nil
}

// fetchConfig reads config.json and maps it to profile.Config with the alias table (config.go). A
// revision without a config yields a nil Config.
func (c *Client) fetchConfig(ctx context.Context, repo, sha string) (*profile.Config, error) {
	path := "/" + repo + "/resolve/" + sha + "/config.json"
	status, body, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, fmt.Errorf("GET %s: %w", path, ErrAccessDenied)
	}
	if status == http.StatusNotFound {
		return nil, nil // no config.json in the revision
	}
	if status/100 != 2 {
		return nil, fmt.Errorf("GET %s: unexpected status %d", path, status)
	}
	cfg, err := ParseConfig(body)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	return cfg, nil
}

// fetchIndexWeightFiles reads model.safetensors.index.json and returns its weight_map's distinct
// file names, sorted.
func (c *Client) fetchIndexWeightFiles(ctx context.Context, repo, sha string) ([]string, error) {
	path := "/" + repo + "/resolve/" + sha + "/" + indexName
	status, body, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	if status/100 != 2 {
		return nil, fmt.Errorf("GET %s: unexpected status %d", path, status)
	}
	var index struct {
		WeightMap map[string]string `json:"weight_map"`
	}
	if err := json.Unmarshal(body, &index); err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	set := make(map[string]bool, len(index.WeightMap))
	for _, file := range index.WeightMap {
		set[file] = true
	}
	files := make([]string, 0, len(set)) // non-nil even with an empty weight_map
	for file := range set {
		files = append(files, file)
	}
	sort.Strings(files)
	return files, nil
}

// get issues GET BaseURL+path (with the token, if any) and returns the status and body.
func (c *Client) get(ctx context.Context, path string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.BaseURL, "/")+path, nil)
	if err != nil {
		return 0, nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// LargestFetchFile is the size of the largest file the fetch job will copy from m (the files
// platform.FetchFileAllowed accepts). The fetch Job's volume is sized from it.
func LargestFetchFile(m profile.Model) int64 {
	var largest int64
	for _, f := range m.Files {
		if platform.FetchFileAllowed(f.Name) && f.Size > largest {
			largest = f.Size
		}
	}
	return largest
}
