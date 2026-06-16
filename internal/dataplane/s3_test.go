package dataplane

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mackee/localfront/internal/config"
	"github.com/mackee/localfront/internal/origin"
)

// ─────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeFetcher is a programmable origin.Fetcher for tests.
type fakeFetcher struct {
	lastReq *origin.Request
	resp    *origin.Response
	err     error
}

func (f *fakeFetcher) Fetch(_ context.Context, req *origin.Request) (*origin.Response, error) {
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	resp := f.resp
	if resp == nil {
		return &origin.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	}
	// Snapshot the body so calls to bodyStringOnce don't drain it for future calls.
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(strings.NewReader(string(b)))
	return &origin.Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header.Clone(),
		Body:       io.NopCloser(strings.NewReader(string(b))),
	}, nil
}

var _ origin.Fetcher = (*fakeFetcher)(nil)

// s3TestOrigin builds a minimal S3 origin.
func s3TestOrigin(id, bucket, originPath string) *config.Origin {
	return &config.Origin{
		ID:         id,
		OriginPath: originPath,
		S3: &config.S3Origin{
			Bucket: bucket,
			Region: "us-east-1",
		},
	}
}

// s3TestDistribution builds a minimal S3-backed distribution.
func s3TestDistribution(domainName, defaultRootObject string, o *config.Origin) *config.Distribution {
	beh := &config.Behavior{
		Origin:         o,
		AllowedMethods: []string{"GET", "HEAD"},
		CachePolicy:    &config.CachePolicy{},
	}
	return &config.Distribution{
		LogicalID:         "D1",
		ID:                "D1",
		DomainName:        domainName,
		Aliases:           []string{"assets.example.test"},
		Enabled:           true,
		DefaultRootObject: defaultRootObject,
		Origins:           []*config.Origin{o},
		DefaultBehavior:   beh,
	}
}

// newS3CFRequest builds a test HTTP request with Host set for the S3 tests.
func newS3CFRequest(method, host, path string) *http.Request {
	req := httptest.NewRequest(method, "http://"+host+path, nil)
	req.Host = host
	return req
}

// newS3Server builds a Server with the given fake fetcher.
func newS3Server(cfg *config.Config, fake *fakeFetcher) *Server {
	return New(cfg, discardLogger(), WithS3Fetcher(fake))
}

// ─────────────────────────────────────────────────
// B1: s3Key table test (unexported, accessible from same package)
// ─────────────────────────────────────────────────

func TestS3Key(t *testing.T) {
	tests := []struct {
		originPath        string
		urlPath           string
		defaultRootObject string
		want              string
	}{
		{"", "/", "index.html", "index.html"},
		{"", "/app.js", "index.html", "app.js"},
		{"", "/dir/", "index.html", "dir/"},
		{"/static", "/app.js", "index.html", "static/app.js"},
		{"/static", "/", "index.html", "static/index.html"},
		{"", "", "index.html", "index.html"},
		{"", "/x", "", "x"},
	}
	for _, tc := range tests {
		got := s3Key(tc.originPath, tc.urlPath, tc.defaultRootObject)
		if got != tc.want {
			t.Errorf("s3Key(%q, %q, %q) = %q, want %q",
				tc.originPath, tc.urlPath, tc.defaultRootObject, got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────
// B8: cacheStatus table test (unexported)
// ─────────────────────────────────────────────────

func TestS3CacheStatus(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{200, "Miss from localfront"},
		{206, "Miss from localfront"},
		{304, "Miss from localfront"},
		{399, "Miss from localfront"},
		{400, "Error from localfront"},
		{403, "Error from localfront"},
		{404, "Error from localfront"},
		{500, "Error from localfront"},
	}
	for _, tc := range tests {
		got := cacheStatus(tc.status)
		if got != tc.want {
			t.Errorf("cacheStatus(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────
// B2: Basic GET / → key "index.html", response passthrough + CF headers
// ─────────────────────────────────────────────────

func TestS3Proxy_GetRootDefaultObject(t *testing.T) {
	o := s3TestOrigin("s3", "assets", "")
	dist := s3TestDistribution("assets.cloudfront.localhost", "index.html", o)
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}

	fake := &fakeFetcher{
		resp: &origin.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("hi")),
		},
	}
	srv := newS3Server(cfg, fake)

	req := newS3CFRequest(http.MethodGet, "assets.cloudfront.localhost", "/")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if fake.lastReq == nil {
		t.Fatal("fetcher was not called")
	}
	if fake.lastReq.Bucket != "assets" {
		t.Errorf("bucket = %q, want assets", fake.lastReq.Bucket)
	}
	if fake.lastReq.Key != "index.html" {
		t.Errorf("key = %q, want index.html", fake.lastReq.Key)
	}
	if fake.lastReq.Method != "GET" {
		t.Errorf("method = %q, want GET", fake.lastReq.Method)
	}
	if body := rr.Body.String(); body != "hi" {
		t.Errorf("body = %q, want hi", body)
	}
	if got := rr.Header().Get("X-Cache"); got != "Miss from localfront" {
		t.Errorf("X-Cache = %q, want 'Miss from localfront'", got)
	}
	cfID := rr.Header().Get("X-Amz-Cf-Id")
	if len(cfID) != 56 {
		t.Errorf("X-Amz-Cf-Id length = %d, want 56, value = %q", len(cfID), cfID)
	}
	if got := rr.Header().Get("X-Amz-Cf-Pop"); got == "" {
		t.Error("X-Amz-Cf-Pop missing from response")
	}
	via := rr.Header().Get("Via")
	if !strings.Contains(via, "1.1 assets.cloudfront.localhost (CloudFront)") {
		t.Errorf("Via = %q, want to contain '1.1 assets.cloudfront.localhost (CloudFront)'", via)
	}
}

// ─────────────────────────────────────────────────
// B3: Forwarded headers — Range and If-None-Match reach origin, Cookie does not
// ─────────────────────────────────────────────────

func TestS3Proxy_ForwardedHeaders(t *testing.T) {
	o := s3TestOrigin("s3", "assets", "")
	dist := s3TestDistribution("assets.cloudfront.localhost", "index.html", o)
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}

	fake := &fakeFetcher{
		resp: &origin.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		},
	}
	srv := newS3Server(cfg, fake)

	req := newS3CFRequest(http.MethodGet, "assets.cloudfront.localhost", "/file.bin")
	req.Header.Set("Range", "bytes=0-1023")
	req.Header.Set("If-None-Match", `"abc"`)
	req.Header.Set("Cookie", "session=xyz")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if fake.lastReq == nil {
		t.Fatal("fetcher was not called")
	}
	if got := fake.lastReq.Headers.Get("Range"); got != "bytes=0-1023" {
		t.Errorf("Range = %q, want bytes=0-1023", got)
	}
	if got := fake.lastReq.Headers.Get("If-None-Match"); got != `"abc"` {
		t.Errorf("If-None-Match = %q, want \"abc\"", got)
	}
	if got := fake.lastReq.Headers.Get("Cookie"); got != "" {
		t.Errorf("Cookie should not be forwarded, got %q", got)
	}
}

// ─────────────────────────────────────────────────
// Parity with the custom-origin path: query string, Accept-Encoding, and
// origin custom headers reach the S3 store; hop-by-hop headers do not.
// ─────────────────────────────────────────────────

func TestS3Proxy_ForwardsQueryAcceptEncodingAndCustomHeaders(t *testing.T) {
	o := s3TestOrigin("s3", "assets", "")
	o.CustomHeaders = []config.Header{{Name: "X-Origin-Token", Value: "secret"}}
	dist := s3TestDistribution("assets.cloudfront.localhost", "", o)
	// Forward all query strings and normalize Accept-Encoding to gzip.
	dist.DefaultBehavior.CachePolicy = &config.CachePolicy{
		Gzip:         true,
		QueryStrings: config.ListSelection{Behavior: "all"},
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}

	fake := &fakeFetcher{}
	srv := newS3Server(cfg, fake)

	req := newS3CFRequest(http.MethodGet, "assets.cloudfront.localhost", "/file.bin?versionId=v2&x=1")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if fake.lastReq == nil {
		t.Fatal("fetcher was not called")
	}
	if got := fake.lastReq.RawQuery; got != "versionId=v2&x=1" {
		t.Errorf("RawQuery = %q, want versionId=v2&x=1", got)
	}
	if got := fake.lastReq.Headers.Get("Accept-Encoding"); got != "gzip" {
		t.Errorf("Accept-Encoding = %q, want gzip", got)
	}
	if got := fake.lastReq.Headers.Get("X-Origin-Token"); got != "secret" {
		t.Errorf("origin custom header X-Origin-Token = %q, want secret", got)
	}
}

func TestS3Proxy_StripsHopByHopHeaders(t *testing.T) {
	o := s3TestOrigin("s3", "assets", "")
	dist := s3TestDistribution("assets.cloudfront.localhost", "", o)
	// allViewer forwards every viewer header into BuildOriginRequest's output,
	// including hop-by-hop ones; the data plane must strip them before signing.
	dist.DefaultBehavior.OriginRequestPolicy = &config.OriginRequestPolicy{
		Headers: config.ListSelection{Behavior: "allViewer"},
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}

	fake := &fakeFetcher{}
	srv := newS3Server(cfg, fake)

	req := newS3CFRequest(http.MethodGet, "assets.cloudfront.localhost", "/file.bin")
	req.Header.Set("Connection", "X-Drop-Me")
	req.Header.Set("X-Drop-Me", "1")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("X-Keep-Me", "ok")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if fake.lastReq == nil {
		t.Fatal("fetcher was not called")
	}
	for _, h := range []string{"Connection", "X-Drop-Me", "Keep-Alive"} {
		if got := fake.lastReq.Headers.Get(h); got != "" {
			t.Errorf("hop-by-hop header %s should be stripped, got %q", h, got)
		}
	}
	if got := fake.lastReq.Headers.Get("X-Keep-Me"); got != "ok" {
		t.Errorf("X-Keep-Me = %q, want ok (non-hop-by-hop must still pass)", got)
	}
}

// ─────────────────────────────────────────────────
// B4: Error and passthrough status codes
// ─────────────────────────────────────────────────

func TestS3Proxy_StatusPassthrough(t *testing.T) {
	tests := []struct {
		name          string
		fakeStatus    int
		fakeHeader    http.Header
		wantStatus    int
		wantXCache    string
		checkRangeHdr bool
	}{
		{
			name:       "404 → Error from localfront",
			fakeStatus: http.StatusNotFound,
			fakeHeader: make(http.Header),
			wantStatus: http.StatusNotFound,
			wantXCache: "Error from localfront",
		},
		{
			name:       "403 → Error from localfront",
			fakeStatus: http.StatusForbidden,
			fakeHeader: make(http.Header),
			wantStatus: http.StatusForbidden,
			wantXCache: "Error from localfront",
		},
		{
			name:       "206 passthrough → Miss from localfront",
			fakeStatus: http.StatusPartialContent,
			fakeHeader: func() http.Header {
				h := make(http.Header)
				h.Set("Content-Range", "bytes 0-9/100")
				return h
			}(),
			wantStatus:    http.StatusPartialContent,
			wantXCache:    "Miss from localfront",
			checkRangeHdr: true,
		},
		{
			name:       "304 → Miss from localfront",
			fakeStatus: http.StatusNotModified,
			fakeHeader: make(http.Header),
			wantStatus: http.StatusNotModified,
			wantXCache: "Miss from localfront",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := s3TestOrigin("s3", "assets", "")
			dist := s3TestDistribution("assets.cloudfront.localhost", "", o)
			cfg := &config.Config{Distributions: []*config.Distribution{dist}}

			fake := &fakeFetcher{
				resp: &origin.Response{
					StatusCode: tc.fakeStatus,
					Header:     tc.fakeHeader,
					Body:       io.NopCloser(strings.NewReader("")),
				},
			}
			srv := newS3Server(cfg, fake)

			req := newS3CFRequest(http.MethodGet, "assets.cloudfront.localhost", "/file.txt")
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rr.Code, tc.wantStatus)
			}
			if got := rr.Header().Get("X-Cache"); got != tc.wantXCache {
				t.Errorf("X-Cache = %q, want %q", got, tc.wantXCache)
			}
			if tc.checkRangeHdr {
				if got := rr.Header().Get("Content-Range"); got != "bytes 0-9/100" {
					t.Errorf("Content-Range = %q, want bytes 0-9/100", got)
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────
// B5: HEAD request — body not copied
// ─────────────────────────────────────────────────

func TestS3Proxy_HEADRequest(t *testing.T) {
	o := s3TestOrigin("s3", "assets", "")
	dist := s3TestDistribution("assets.cloudfront.localhost", "", o)
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}

	fake := &fakeFetcher{
		resp: &origin.Response{
			StatusCode: http.StatusOK,
			Header: func() http.Header {
				h := make(http.Header)
				h.Set("Content-Length", "42")
				return h
			}(),
			Body: io.NopCloser(strings.NewReader("should not appear")),
		},
	}
	srv := newS3Server(cfg, fake)

	req := newS3CFRequest(http.MethodHead, "assets.cloudfront.localhost", "/file.bin")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); body != "" {
		t.Errorf("HEAD response body should be empty, got %q", body)
	}
	if got := rr.Header().Get("Content-Length"); got != "42" {
		t.Errorf("Content-Length = %q, want 42", got)
	}
	if got := rr.Header().Get("X-Cache"); got != "Miss from localfront" {
		t.Errorf("X-Cache = %q, want 'Miss from localfront'", got)
	}
}

// ─────────────────────────────────────────────────
// B6: Fetch error → 502 CloudFront error page
// ─────────────────────────────────────────────────

func TestS3Proxy_FetchError(t *testing.T) {
	o := s3TestOrigin("s3", "assets", "")
	dist := s3TestDistribution("assets.cloudfront.localhost", "", o)
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}

	fake := &fakeFetcher{err: context.DeadlineExceeded}
	srv := newS3Server(cfg, fake)

	req := newS3CFRequest(http.MethodGet, "assets.cloudfront.localhost", "/file.txt")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rr.Code)
	}
	if got := rr.Header().Get("Server"); got != "CloudFront" {
		t.Errorf("Server = %q, want CloudFront", got)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "could not be satisfied") {
		t.Errorf("error page body does not contain 'could not be satisfied': %q", body)
	}
}

// ─────────────────────────────────────────────────
// B7: No fetcher configured → 502 mentioning --s3-endpoint
// ─────────────────────────────────────────────────

func TestS3Proxy_NoFetcherConfigured(t *testing.T) {
	o := s3TestOrigin("s3", "assets", "")
	dist := s3TestDistribution("assets.cloudfront.localhost", "", o)
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}

	// Do NOT pass WithS3Fetcher
	srv := New(cfg, discardLogger())

	req := newS3CFRequest(http.MethodGet, "assets.cloudfront.localhost", "/file.txt")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rr.Code)
	}
	body := strings.ToLower(rr.Body.String())
	if !strings.Contains(body, "--s3-endpoint") {
		t.Errorf("error page body does not mention --s3-endpoint: %q", rr.Body.String())
	}
}
