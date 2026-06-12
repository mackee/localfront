package dataplane

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mackee/localfront/internal/config"
	"github.com/mackee/localfront/internal/origin"
)

// s3RequestTimeout bounds a single origin fetch from the object store.
const s3RequestTimeout = 30 * time.Second

func (s *Server) proxyS3Origin(w http.ResponseWriter, r *http.Request, dist *config.Distribution, behavior *config.Behavior, requestID string) {
	if s.s3 == nil {
		s.logger.Error("S3 origin requested but no object store is configured", "origin", behavior.Origin.ID)
		writeCFError(w, http.StatusBadGateway, requestID,
			"This distribution has an S3 origin, but localfront was started without an S3 store (set --s3-endpoint).")
		return
	}
	o := behavior.Origin
	key := s3Key(o.OriginPath, r.URL.Path, dist.DefaultRootObject)

	ctx, cancel := context.WithTimeout(r.Context(), s3RequestTimeout)
	defer cancel()

	resp, err := s.s3.Fetch(ctx, &origin.Request{
		Bucket:  o.S3.Bucket,
		Key:     key,
		Method:  r.Method,
		Headers: origin.CollectForwardedHeaders(r.Header),
	})
	if err != nil {
		s.logger.Error("S3 origin fetch failed", "origin", o.ID, "bucket", o.S3.Bucket, "key", key, "error", err)
		writeCFError(w, http.StatusBadGateway, requestID,
			"localfront wasn't able to fetch the object from the S3 origin.")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	copyHeaders(w.Header(), resp.Header)
	removeHopByHopHeaders(w.Header())
	addVia(w.Header(), dist.DomainName)
	w.Header().Set("X-Cache", s3CacheStatus(resp.StatusCode))
	w.Header().Set("X-Amz-Cf-Id", requestID)
	w.Header().Set("X-Amz-Cf-Pop", popName)
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		if _, err := io.Copy(w, resp.Body); err != nil {
			s.logger.Debug("copying S3 response failed", "error", err)
		}
	}
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

func s3CacheStatus(status int) string {
	if status >= 400 {
		return "Error from localfront"
	}
	return "Miss from localfront"
}
