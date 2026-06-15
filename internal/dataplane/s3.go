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
	// Range and conditional headers.
	headers := http.Header{}
	for name, values := range originReq.Headers {
		headers[name] = append([]string(nil), values...)
	}
	addAlwaysForwardedHeaders(headers, r.Header)

	reqCtx, cancel := context.WithTimeout(ctx, s3RequestTimeout)
	resp, err := s.s3.Fetch(reqCtx, &origin.Request{
		Bucket:  o.S3.Bucket,
		Key:     key,
		Method:  r.Method,
		Headers: headers,
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
	if (urlPath == "" || urlPath == "/") && defaultRootObject != "" {
		urlPath = "/" + defaultRootObject
	}
	return strings.TrimPrefix(originPath+urlPath, "/")
}
