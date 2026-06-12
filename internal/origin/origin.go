// Package origin fetches objects from distribution origins. The PoC supports
// S3 origins backed by an external S3-compatible object store; custom HTTP
// origins are proxied directly by the data plane.
package origin

import (
	"context"
	"io"
	"net/http"
)

// Request is a fetch for one object from an S3 origin.
type Request struct {
	Bucket string
	Key    string
	Method string // "GET" or "HEAD"
	// Headers are viewer headers forwarded to the store unchanged: Range and
	// the conditional headers (If-None-Match, If-Modified-Since, ...).
	Headers http.Header
}

// Response is the store's reply, passed through to the viewer.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
}

// Fetcher fetches objects from an S3-compatible store.
type Fetcher interface {
	Fetch(ctx context.Context, req *Request) (*Response, error)
}

// forwardedRequestHeaders are the viewer headers CloudFront forwards to an S3
// origin (besides the ones it sets itself); everything else is dropped.
var forwardedRequestHeaders = []string{
	"Range",
	"If-Match",
	"If-None-Match",
	"If-Modified-Since",
	"If-Unmodified-Since",
}

// CollectForwardedHeaders extracts the headers that should reach an S3 origin.
func CollectForwardedHeaders(src http.Header) http.Header {
	var out http.Header
	for _, name := range forwardedRequestHeaders {
		if v := src.Values(name); len(v) > 0 {
			if out == nil {
				out = make(http.Header)
			}
			out[http.CanonicalHeaderKey(name)] = append([]string(nil), v...)
		}
	}
	return out
}
