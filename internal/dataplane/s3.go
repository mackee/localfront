package dataplane

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/mackee/localfront/internal/behavior"
	"github.com/mackee/localfront/internal/config"
	"github.com/mackee/localfront/internal/origin"
)

// s3RequestTimeout bounds a single origin fetch from the object store.
const s3RequestTimeout = 30 * time.Second

func (s *Server) fetchS3Origin(ctx context.Context, r *http.Request, dist *config.Distribution, beh *config.Behavior, originReq behavior.OriginRequest, requestID string) *originResponse {
	if s.s3 == nil {
		s.logger.Error("S3 origin requested but no object store is configured", "origin", beh.Origin.ID)
		return s.s3UnconfiguredResponse()
	}
	o := beh.Origin
	key := s3Key(o.OriginPath, r.URL.Path, dist.DefaultRootObject)

	// S3 origins receive the policy-selected headers plus the always-forwarded
	// Range and conditional headers, mirroring the custom-origin path.
	headers := http.Header{}
	for name, values := range originReq.Headers {
		headers[name] = append([]string(nil), values...)
	}
	addAlwaysForwardedHeaders(headers, r.Header)
	removeHopByHopHeaders(headers)
	if originReq.AcceptEncoding != "" {
		headers.Set("Accept-Encoding", originReq.AcceptEncoding)
	}
	// Static origin custom headers reach S3 origins too, like custom origins.
	for _, h := range o.CustomHeaders {
		headers.Set(h.Name, h.Value)
	}

	reqCtx, cancel := context.WithTimeout(ctx, s3RequestTimeout)
	resp, err := s.s3.Fetch(reqCtx, &origin.Request{
		Bucket:   o.S3.Bucket,
		Key:      key,
		Method:   r.Method,
		RawQuery: originReq.RawQuery,
		Headers:  headers,
	})
	if err != nil {
		cancel()
		s.logger.Error("S3 origin fetch failed", "origin", o.ID, "bucket", o.S3.Bucket, "key", key, "error", err)
		return nil
	}
	return &originResponse{
		statusCode: resp.StatusCode,
		header:     resp.Header,
		body:       &cancelOnClose{ReadCloser: resp.Body, cancel: cancel},
	}
}

// s3UnconfiguredResponse synthesizes the 502 served when a distribution has an
// S3 origin but localfront was started without an object store.
func (s *Server) s3UnconfiguredResponse() *originResponse {
	const msg = "This distribution has an S3 origin, but localfront was started without an S3 store (set --s3-endpoint)."
	body, header := cfErrorPage(http.StatusBadGateway, "", msg)
	return &originResponse{statusCode: http.StatusBadGateway, header: header, body: body}
}

// s3Key derives the object key from the origin path and request path, applying
// the default root object only at the distribution root (CloudFront does not
// append it to subdirectory paths).
func s3Key(originPath, urlPath, defaultRootObject string) string {
	return strings.TrimPrefix(originPath+applyDefaultRootObject(urlPath, defaultRootObject), "/")
}

// applyDefaultRootObject substitutes the default root object for a request at
// the distribution root ("" or "/"). CloudFront applies it for every origin
// type, but only at the root, never to subdirectory paths.
func applyDefaultRootObject(urlPath, defaultRootObject string) string {
	if (urlPath == "" || urlPath == "/") && defaultRootObject != "" {
		return "/" + defaultRootObject
	}
	return urlPath
}
