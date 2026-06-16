package dataplane

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/mackee/localfront/internal/behavior"
	"github.com/mackee/localfront/internal/config"
	"github.com/mackee/localfront/internal/sign"
)

// originResponse is an origin reply held in the pipeline before it is written
// to the viewer, so custom error responses, the response headers policy, and
// compression can post-process it.
type originResponse struct {
	statusCode int
	header     http.Header
	body       io.ReadCloser
}

func (o *originResponse) close() {
	if o != nil && o.body != nil {
		_ = o.body.Close()
	}
}

// alwaysForwardedHeaders reach the origin regardless of the cache / origin
// request policies: CloudFront forwards Range and conditional headers so that
// range and conditional requests work without policy configuration.
var alwaysForwardedHeaders = []string{
	"Range",
	"If-Match",
	"If-None-Match",
	"If-Modified-Since",
	"If-Unmodified-Since",
}

// serve runs the request pipeline for a resolved distribution and behavior:
// the viewer-request function, the origin request build and fetch, custom error
// responses, the viewer-response function, the response headers policy, and
// compression, then writes the response with CloudFront headers.
func (s *Server) serve(w http.ResponseWriter, r *http.Request, dist *config.Distribution, beh *config.Behavior, snap *snapshot, requestID string) {
	// Signed URL / cookie verification for behaviors with trusted key groups.
	// Fidelity note 1: the order relative to viewer-request functions (which
	// could rewrite the URI the canned policy signs) is still to be confirmed
	// against a live distribution; localfront verifies before the function runs.
	if len(beh.TrustedKeyGroups) > 0 {
		if err := sign.Verify(r, snap.trustedKeys(beh), time.Now(), dist.DefaultRootObject, s.publicHost); err != nil {
			s.logger.Info("signed URL/cookie verification failed", "distribution", dist.LogicalID, "reason", err)
			writeCFError(w, http.StatusForbidden, requestID, "Access denied: "+err.Error())
			return
		}
	}

	viewerHeaders := behavior.SynthesizeViewerHeaders(r)

	// viewer-request function: may rewrite the request or return a response.
	var resp *originResponse
	if beh.ViewerRequest != nil {
		shortCircuit, ok := s.runViewerRequest(w, r, dist, beh, snap, viewerHeaders, requestID)
		if !ok {
			return // a 503 was already written
		}
		resp = shortCircuit
	}

	if resp == nil {
		// Behavior is intentionally NOT re-evaluated after a function rewrites
		// the URI (CloudFront runs functions after behavior selection).
		originReq := behavior.BuildOriginRequest(beh, r, viewerHeaders)
		resp = s.fetchOrigin(r.Context(), r, dist, beh, originReq, requestID)
		if resp == nil {
			writeCFError(w, http.StatusBadGateway, requestID, "localfront wasn't able to connect to the origin.")
			return
		}
		if applied := s.applyErrorResponse(r.Context(), r, dist, resp, viewerHeaders, requestID); applied != nil {
			resp.close()
			resp = applied
		}
		// CloudFront Functions are not invoked for origin responses with a 4xx/5xx
		// status, so skip the viewer-response function unless a custom error
		// response above already rewrote the status below 400.
		if beh.ViewerResponse != nil && resp.statusCode < 400 {
			if !s.runViewerResponse(w, r, dist, beh, snap, viewerHeaders, resp, requestID) {
				resp.close()
				return // a 503 was already written
			}
		}
	}
	defer func() { resp.close() }()

	behavior.ApplyResponseHeaders(beh, resp.header, r.Header.Get("Origin"))
	s.writeResponse(w, r, dist, beh, resp, requestID)
}

// fetchOrigin dispatches to the custom or S3 origin fetch for the behavior's
// origin, returning nil on a transport-level failure (already logged).
func (s *Server) fetchOrigin(ctx context.Context, r *http.Request, dist *config.Distribution, beh *config.Behavior, originReq behavior.OriginRequest, requestID string) *originResponse {
	o := beh.Origin
	switch {
	case o.Custom != nil:
		return s.fetchCustomOrigin(ctx, r, dist, beh, originReq, requestID)
	case o.S3 != nil:
		return s.fetchS3Origin(ctx, r, dist, beh, originReq, requestID)
	default:
		s.logger.Error("origin has no usable configuration", "origin", o.ID)
		return nil
	}
}

// applyErrorResponse implements custom error responses. When the origin status
// matches a configured ErrorCode, it either rewrites the status code (no
// ResponsePagePath) or fetches the response page from the behavior that serves
// that path and returns it with the configured ResponseCode. It returns nil
// when no error response applies (resp is left untouched).
func (s *Server) applyErrorResponse(ctx context.Context, r *http.Request, dist *config.Distribution, resp *originResponse, viewerHeaders http.Header, requestID string) *originResponse {
	er := matchErrorResponse(dist, resp.statusCode)
	if er == nil {
		return nil
	}
	if er.ResponsePagePath == "" {
		// No page: just rewrite the status code, keep the origin body.
		rewritten := &originResponse{
			statusCode: er.ResponseCode,
			header:     resp.header.Clone(),
			body:       resp.body,
		}
		resp.body = nil // ownership moved to rewritten
		return rewritten
	}

	pageBeh := behavior.Select(dist, er.ResponsePagePath)
	pageReq := r.Clone(ctx)
	pageReq.Method = http.MethodGet
	pageReq.Body = nil
	pageReq.ContentLength = 0
	// The error page is an unconditioned GET of a different resource: drop the
	// viewer's Range and conditional headers so the page origin can't answer
	// 206/304 against the error page (which is then served with ResponseCode).
	for _, h := range alwaysForwardedHeaders {
		pageReq.Header.Del(h)
	}
	pageURL := *r.URL
	pageURL.Path = er.ResponsePagePath
	pageURL.RawPath = ""
	pageReq.URL = &pageURL

	// The error page is fetched as the viewer request re-evaluated under the
	// behavior that serves ResponsePagePath: its cache / origin request policies
	// (not the failed behavior's) decide what reaches the origin. Reusing the
	// failed behavior's origin request would leak its forwarded headers/query.
	pageOriginReq := behavior.BuildOriginRequest(pageBeh, pageReq, viewerHeaders)
	page := s.fetchOrigin(ctx, pageReq, dist, pageBeh, pageOriginReq, requestID)
	if page == nil {
		s.logger.Warn("custom error page fetch failed; serving origin error",
			"errorCode", er.ErrorCode, "page", er.ResponsePagePath)
		return nil
	}
	// Serve the page body with the configured response code.
	page.statusCode = er.ResponseCode
	return page
}

func matchErrorResponse(dist *config.Distribution, status int) *config.ErrorResponse {
	for _, er := range dist.ErrorResponses {
		if er.ErrorCode == status {
			return er
		}
	}
	return nil
}

const customOriginDialTimeout = 10 * time.Second
