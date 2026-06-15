package dataplane_test

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mackee/localfront/internal/config"
	"github.com/mackee/localfront/internal/dataplane"
)

// customOrigin builds a custom HTTP origin pointing at host:port.
func customOrigin(id, host string, port int) *config.Origin {
	return &config.Origin{
		ID: id,
		Custom: &config.CustomOrigin{
			Host:             host,
			HTTPPort:         port,
			ProtocolPolicy:   "http-only",
			ReadTimeout:      5 * time.Second,
			KeepaliveTimeout: 5 * time.Second,
		},
	}
}

func getHeadBehavior(o *config.Origin, pattern string) *config.Behavior {
	return &config.Behavior{
		PathPattern:    pattern,
		Origin:         o,
		AllowedMethods: []string{"GET", "HEAD"},
		CachePolicy:    &config.CachePolicy{},
	}
}

// ------------------------------------------------------------------
// Path-based routing across cache behaviors and origins
// ------------------------------------------------------------------

func TestM4_PathRoutingAcrossOrigins(t *testing.T) {
	apiSrv, apiHost, apiPort := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "api:"+r.URL.Path)
	})
	_ = apiSrv
	staticSrv, staticHost, staticPort := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "static:"+r.URL.Path)
	})
	_ = staticSrv

	apiOrigin := customOrigin("api", apiHost, apiPort)
	staticOrigin := customOrigin("static", staticHost, staticPort)

	dist := &config.Distribution{
		LogicalID:       "D1",
		ID:              "D1",
		DomainName:      "d1.cloudfront.localhost",
		Aliases:         []string{"site.example.test"},
		Enabled:         true,
		Origins:         []*config.Origin{staticOrigin, apiOrigin},
		DefaultBehavior: getHeadBehavior(staticOrigin, ""),
		Behaviors:       []*config.Behavior{getHeadBehavior(apiOrigin, "/api/*")},
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	tests := []struct {
		path string
		want string
	}{
		{"/api/users", "api:/api/users"},
		{"/index.html", "static:/index.html"},
		{"/", "static:/"},
		{"/api", "static:/api"}, // /api alone does not match /api/*
	}
	for _, tc := range tests {
		req := httptest.NewRequest(http.MethodGet, "http://site.example.test"+tc.path, nil)
		req.Host = "site.example.test"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if got := rr.Body.String(); got != tc.want {
			t.Errorf("path %q served %q, want %q", tc.path, got, tc.want)
		}
	}
}

// ------------------------------------------------------------------
// Custom error responses: SPA fallback 404 -> /index.html (200)
// ------------------------------------------------------------------

func TestM4_CustomErrorResponse_SPAFallback(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.html" {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "<html>spa</html>")
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "origin 404")
	})
	_ = originSrv

	o := customOrigin("o1", host, port)
	dist := &config.Distribution{
		LogicalID:       "D1",
		ID:              "D1",
		DomainName:      "d1.cloudfront.localhost",
		Aliases:         []string{"spa.example.test"},
		Enabled:         true,
		Origins:         []*config.Origin{o},
		DefaultBehavior: getHeadBehavior(o, ""),
		ErrorResponses: []*config.ErrorResponse{
			{ErrorCode: 404, ResponseCode: 200, ResponsePagePath: "/index.html"},
		},
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	req := httptest.NewRequest(http.MethodGet, "http://spa.example.test/app/route", nil)
	req.Host = "spa.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (SPA fallback)", rr.Code)
	}
	if body := rr.Body.String(); body != "<html>spa</html>" {
		t.Errorf("body = %q, want the index.html body", body)
	}
}

// The custom error page is fetched under the behavior that serves
// ResponsePagePath, so the failed request's behavior must not leak its
// forwarded query string and headers onto the error-page origin request.
func TestM4_CustomErrorResponse_CrossBehaviorForwarding(t *testing.T) {
	apiSrv, apiHost, apiPort := newTestOriginServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "api 404")
	})
	_ = apiSrv

	var pagePath, pageQuery, pageSecret string
	staticSrv, staticHost, staticPort := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		pagePath = r.URL.Path
		pageQuery = r.URL.RawQuery
		pageSecret = r.Header.Get("X-Api-Secret")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<html>spa</html>")
	})
	_ = staticSrv

	apiOrigin := customOrigin("api", apiHost, apiPort)
	staticOrigin := customOrigin("static", staticHost, staticPort)

	// The /api/* behavior forwards the debug query and X-Api-Secret header; the
	// default behavior (which serves /index.html) forwards neither.
	apiBeh := &config.Behavior{
		PathPattern:    "/api/*",
		Origin:         apiOrigin,
		AllowedMethods: []string{"GET", "HEAD"},
		CachePolicy: &config.CachePolicy{
			Headers:      config.ListSelection{Behavior: "whitelist", Items: []string{"X-Api-Secret"}},
			QueryStrings: config.ListSelection{Behavior: "whitelist", Items: []string{"debug"}},
		},
	}

	dist := &config.Distribution{
		LogicalID:       "D1",
		ID:              "D1",
		DomainName:      "d1.cloudfront.localhost",
		Aliases:         []string{"site.example.test"},
		Enabled:         true,
		Origins:         []*config.Origin{staticOrigin, apiOrigin},
		DefaultBehavior: getHeadBehavior(staticOrigin, ""),
		Behaviors:       []*config.Behavior{apiBeh},
		ErrorResponses: []*config.ErrorResponse{
			{ErrorCode: 404, ResponseCode: 200, ResponsePagePath: "/index.html"},
		},
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	req := httptest.NewRequest(http.MethodGet, "http://site.example.test/api/missing?debug=1", nil)
	req.Host = "site.example.test"
	req.Header.Set("X-Api-Secret", "leak")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SPA fallback)", rr.Code)
	}
	if body := rr.Body.String(); body != "<html>spa</html>" {
		t.Errorf("body = %q, want the index.html body", body)
	}
	if pagePath != "/index.html" {
		t.Errorf("error page fetched %q, want /index.html", pagePath)
	}
	// The default behavior forwards nothing, so the /api/* behavior's debug
	// query and X-Api-Secret header must not reach the error-page origin.
	if pageQuery != "" {
		t.Errorf("error-page origin received query %q, want none", pageQuery)
	}
	if pageSecret != "" {
		t.Errorf("error-page origin received X-Api-Secret %q, want it not forwarded", pageSecret)
	}
}

func TestM4_CustomErrorResponse_StatusRewriteNoPage(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	})
	_ = originSrv

	o := customOrigin("o1", host, port)
	dist := &config.Distribution{
		LogicalID:       "D1",
		ID:              "D1",
		DomainName:      "d1.cloudfront.localhost",
		Aliases:         []string{"site.example.test"},
		Enabled:         true,
		Origins:         []*config.Origin{o},
		DefaultBehavior: getHeadBehavior(o, ""),
		ErrorResponses: []*config.ErrorResponse{
			{ErrorCode: 500, ResponseCode: 503, ResponsePagePath: ""},
		},
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	req := httptest.NewRequest(http.MethodGet, "http://site.example.test/x", nil)
	req.Host = "site.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (rewritten from origin 500)", rr.Code)
	}
	if body := rr.Body.String(); body != "boom" {
		t.Errorf("body = %q, want origin body kept", body)
	}
}

// ------------------------------------------------------------------
// CORS: preflight OPTIONS and actual request
// ------------------------------------------------------------------

func TestM4_CORS_PreflightAndActual(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, "data")
	})
	_ = originSrv

	o := customOrigin("o1", host, port)
	beh := &config.Behavior{
		Origin:         o,
		AllowedMethods: []string{"GET", "HEAD", "OPTIONS"},
		CachePolicy:    &config.CachePolicy{},
		ResponseHeadersPolicy: &config.ResponseHeadersPolicy{
			Cors: &config.CorsConfig{
				AllowOrigins: []string{"*"},
				AllowMethods: []string{"GET", "HEAD", "OPTIONS"},
				AllowHeaders: []string{"*"},
				HasMaxAge:    true,
				MaxAgeSec:    600,
			},
		},
	}
	dist := &config.Distribution{
		LogicalID:       "D1",
		ID:              "D1",
		DomainName:      "d1.cloudfront.localhost",
		Aliases:         []string{"api.example.test"},
		Enabled:         true,
		Origins:         []*config.Origin{o},
		DefaultBehavior: beh,
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	// Preflight.
	pre := httptest.NewRequest(http.MethodOptions, "http://api.example.test/resource", nil)
	pre.Host = "api.example.test"
	pre.Header.Set("Origin", "https://app.example.test")
	pre.Header.Set("Access-Control-Request-Method", "GET")
	preRR := httptest.NewRecorder()
	srv.ServeHTTP(preRR, pre)
	if preRR.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", preRR.Code)
	}
	if got := preRR.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("preflight Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := preRR.Header().Get("Access-Control-Max-Age"); got != "600" {
		t.Errorf("preflight Access-Control-Max-Age = %q, want 600", got)
	}

	// Actual request.
	act := httptest.NewRequest(http.MethodGet, "http://api.example.test/resource", nil)
	act.Host = "api.example.test"
	act.Header.Set("Origin", "https://app.example.test")
	actRR := httptest.NewRecorder()
	srv.ServeHTTP(actRR, act)
	if actRR.Code != http.StatusOK {
		t.Errorf("actual status = %d, want 200", actRR.Code)
	}
	if got := actRR.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("actual Access-Control-Allow-Origin = %q, want *", got)
	}

	// Same-origin request (no Origin header) gets no CORS headers.
	plain := httptest.NewRequest(http.MethodGet, "http://api.example.test/resource", nil)
	plain.Host = "api.example.test"
	plainRR := httptest.NewRecorder()
	srv.ServeHTTP(plainRR, plain)
	if got := plainRR.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("non-CORS request should have no Access-Control-Allow-Origin, got %q", got)
	}
}

// ------------------------------------------------------------------
// Compression
// ------------------------------------------------------------------

func TestM4_Compression(t *testing.T) {
	const bodyText = "<html>" // expanded below to exceed the 1000-byte threshold
	largeHTML := "<html>" + strings.Repeat("x", 2000) + "</html>"

	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/small":
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, bodyText)
		case "/image":
			w.Header().Set("Content-Type", "image/png")
			_, _ = io.WriteString(w, largeHTML)
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, largeHTML)
		}
	})
	_ = originSrv

	o := customOrigin("o1", host, port)
	beh := getHeadBehavior(o, "")
	beh.Compress = true
	dist := &config.Distribution{
		LogicalID:       "D1",
		ID:              "D1",
		DomainName:      "d1.cloudfront.localhost",
		Aliases:         []string{"site.example.test"},
		Enabled:         true,
		Origins:         []*config.Origin{o},
		DefaultBehavior: beh,
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	t.Run("gzip large html", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://site.example.test/large", nil)
		req.Host = "site.example.test"
		req.Header.Set("Accept-Encoding", "gzip, br")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		if got := rr.Header().Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", got)
		}
		gr, err := gzip.NewReader(rr.Body)
		if err != nil {
			t.Fatalf("gzip reader: %v", err)
		}
		decoded, err := io.ReadAll(gr)
		if err != nil {
			t.Fatalf("gunzip: %v", err)
		}
		if string(decoded) != largeHTML {
			t.Errorf("decompressed body mismatch")
		}
		if vary := rr.Header().Get("Vary"); !strings.Contains(vary, "Accept-Encoding") {
			t.Errorf("Vary = %q, want to contain Accept-Encoding", vary)
		}
	})

	t.Run("no accept-encoding leaves body uncompressed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://site.example.test/large", nil)
		req.Host = "site.example.test"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if got := rr.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding = %q, want empty (viewer did not accept encodings)", got)
		}
		if rr.Body.String() != largeHTML {
			t.Errorf("body should be the plain origin body")
		}
	})

	t.Run("small body not compressed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://site.example.test/small", nil)
		req.Host = "site.example.test"
		req.Header.Set("Accept-Encoding", "gzip")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if got := rr.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding = %q, want empty (below size threshold)", got)
		}
	})

	t.Run("non-compressible type not compressed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://site.example.test/image", nil)
		req.Host = "site.example.test"
		req.Header.Set("Accept-Encoding", "gzip")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if got := rr.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding = %q, want empty (image/png)", got)
		}
	})
}

// ------------------------------------------------------------------
// CloudFront-Viewer-* headers reach the origin, overridable per request
// ------------------------------------------------------------------

func TestM4_ViewerHeadersToOrigin(t *testing.T) {
	var gotCountry, gotDesktop string
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotCountry = r.Header.Get("CloudFront-Viewer-Country")
		gotDesktop = r.Header.Get("CloudFront-Is-Desktop-Viewer")
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	o := customOrigin("o1", host, port)
	beh := &config.Behavior{
		Origin:         o,
		AllowedMethods: []string{"GET", "HEAD"},
		CachePolicy:    &config.CachePolicy{},
		OriginRequestPolicy: &config.OriginRequestPolicy{
			Headers: config.ListSelection{
				Behavior: "allViewerAndWhitelistCloudFront",
				Items:    []string{"CloudFront-Viewer-Country", "CloudFront-Is-Desktop-Viewer"},
			},
		},
	}
	dist := &config.Distribution{
		LogicalID:       "D1",
		ID:              "D1",
		DomainName:      "d1.cloudfront.localhost",
		Aliases:         []string{"geo.example.test"},
		Enabled:         true,
		Origins:         []*config.Origin{o},
		DefaultBehavior: beh,
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	req := httptest.NewRequest(http.MethodGet, "http://geo.example.test/", nil)
	req.Host = "geo.example.test"
	req.Header.Set("X-Localfront-Viewer-Country", "JP")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if gotCountry != "JP" {
		t.Errorf("origin CloudFront-Viewer-Country = %q, want JP (overridden via X-Localfront-Viewer-Country)", gotCountry)
	}
	if gotDesktop != "true" {
		t.Errorf("origin CloudFront-Is-Desktop-Viewer = %q, want true (synthesized default)", gotDesktop)
	}
}
