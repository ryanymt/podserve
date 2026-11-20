package kube

// logs.go reads a crash-looping container's previous run (D-029). The pod log endpoint is reached
// with an unversioned REST client, which the dynamic client's config builds the serializer for;
// deps constructs both clients from the same rest.Config.

import (
	"context"
	"strconv"
	"strings"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// logReader is the REST LogReader NewLogReader returns.
type logReader struct {
	client *rest.RESTClient
}

// NewLogReader returns a LogReader for the cluster cfg points at (D-029):
// rest.UnversionedRESTClientFor(dynamic.ConfigFor(cfg)), reading
// GET /api/v1/namespaces/podserve/pods/<POD>/log?container=<C>&previous=true&tailLines=<N>.
func NewLogReader(cfg *rest.Config) (LogReader, error) {
	client, err := rest.UnversionedRESTClientFor(dynamic.ConfigFor(cfg))
	if err != nil {
		return nil, err
	}
	return logReader{client: client}, nil
}

// PreviousLog reads the last lines of a container's previous run, one element per line and no
// trailing empty one. A non-2xx answer (the API server's 400 when there is no previous run) is an
// error.
func (r logReader) PreviousLog(ctx context.Context, pod, container string, lines int64) ([]string, error) {
	raw, err := r.client.Get().
		AbsPath("/api/v1/namespaces/podserve/pods", pod, "log").
		Param("container", container).
		Param("previous", "true").
		Param("tailLines", strconv.FormatInt(lines, 10)).
		Do(ctx).Raw()
	if err != nil {
		return nil, err
	}
	if body := strings.TrimRight(string(raw), "\n"); body != "" {
		return strings.Split(body, "\n"), nil
	}
	return nil, nil
}
