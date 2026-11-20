package kube

import (
	"errors"
	"io"
	"net"
	"syscall"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// transient reports an error a poll loop should wait through rather than fail on: the API server
// being briefly unable to answer (500, 503, 504, 429, a server-side timeout) or the connection
// dropping. Found live in P02 T9: one "the server is currently unable to handle the request" while
// listing ReplicaSets ended a 17-minute deploy with exit 1. Every other error (forbidden, not found,
// bad request) is real and returned at once.
func transient(err error) bool {
	if err == nil {
		return false
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		switch status.Status().Code {
		case 429, 500, 502, 503, 504:
			return true
		}
		return apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err)
	}
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED)
}
