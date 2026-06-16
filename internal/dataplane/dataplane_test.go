package dataplane_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mackee/localfront/internal/config"
	"github.com/mackee/localfront/internal/dataplane"
)

// newLogger returns a no-op logger suitable for tests.
func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// baseOrigin builds a minimal *config.Origin pointing at addr (host:port string).
func baseOrigin(id, host string, port int) *config.Origin {
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

// baseBehavior builds a minimal GET/HEAD behavior wired to origin.
func baseBehavior(origin *config.Origin) *config.Behavior {
	return &config.Behavior{
		Origin:         origin,
		AllowedMethods: []string{"GET", "HEAD"},
		CachePolicy: &config.CachePolicy{
			MinTTL:     0,
			DefaultTTL: 0,
			MaxTTL:     0,
		},
	}
}

// baseDistribution builds a minimal, enabled distribution.
func baseDistribution(logicalID, domainName string, aliases []string, origin *config.Origin) *config.Distribution {
	beh := baseBehavior(origin)
	return &config.Distribution{
		LogicalID:       logicalID,
		ID:              logicalID,
		DomainName:      domainName,
		Aliases:         aliases,
		Enabled:         true,
		Origins:         []*config.Origin{origin},
		DefaultBehavior: beh,
	}
}

// newTestOriginServer creates an httptest.Server and parses host/port from its URL.
func newTestOriginServer(t *testing.T, handler http.HandlerFunc) (srv *httptest.Server, host string, port int) {
	t.Helper()
	srv = httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	h, p, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split host/port: %v", err)
	}
	portInt := 0
	_, _ = net.ResolveTCPAddr("tcp", srv.Listener.Addr().String()) // validation
	for _, b := range []byte(p) {
		portInt = portInt*10 + int(b-'0')
	}
	return srv, h, portInt
}

// parsePort parses a decimal port string.
func parsePort(s string) int {
	n := 0
	for _, b := range []byte(s) {
		n = n*10 + int(b-'0')
	}
	return n
}

// ------------------------------------------------------------------
// 1. Host routing
// ------------------------------------------------------------------

func TestHostRouting_ExactAlias(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	origin := baseOrigin("o1", host, port)
	dist := baseDistribution("D1", "d1.cloudfront.localhost", []string{"assets.example.test", "static.example.test"}, origin)
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	tests := []struct {
		name string
		host string
		want int
	}{
		{"alias match", "assets.example.test", http.StatusOK},
		{"second alias", "static.example.test", http.StatusOK},
		{"domain name", "d1.cloudfront.localhost", http.StatusOK},
		{"alias uppercase", "ASSETS.EXAMPLE.TEST", http.StatusOK},
		{"alias with port", "assets.example.test:8080", http.StatusOK},
		{"unknown host", "unknown.example.test", http.StatusForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/", nil)
			req.Host = tc.host
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Errorf("status = %d, want %d", rr.Code, tc.want)
			}
		})
	}
}

func TestHostRouting_UnknownHost_CFHeaders(t *testing.T) {
	cfg := &config.Config{Distributions: []*config.Distribution{}}
	srv := dataplane.New(cfg, newLogger())

	req := httptest.NewRequest(http.MethodGet, "http://nope.example.test/", nil)
	req.Host = "nope.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
	if got := rr.Header().Get("Server"); got != "CloudFront" {
		t.Errorf("Server = %q, want CloudFront", got)
	}
	if got := rr.Header().Get("X-Cache"); got != "Error from localfront" {
		t.Errorf("X-Cache = %q, want Error from localfront", got)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "could not be satisfied") {
		t.Errorf("body does not contain 'could not be satisfied': %q", body)
	}
}

func TestHostRouting_WildcardAlias(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	origin := baseOrigin("o1", host, port)
	dist := baseDistribution("D1", "d1.cloudfront.localhost", []string{"*.cdn.example.test"}, origin)
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	tests := []struct {
		name string
		host string
		want int
	}{
		{"single label match", "a.cdn.example.test", http.StatusOK},
		{"another single label", "x.cdn.example.test", http.StatusOK},
		{"two extra labels no match", "a.b.cdn.example.test", http.StatusForbidden},
		{"bare base no match", "cdn.example.test", http.StatusForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/", nil)
			req.Host = tc.host
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Errorf("host=%q status=%d want=%d", tc.host, rr.Code, tc.want)
			}
		})
	}
}

func TestHostRouting_DisabledDistribution(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	origin := baseOrigin("o1", host, port)
	beh := baseBehavior(origin)
	dist := &config.Distribution{
		LogicalID:       "D1",
		ID:              "D1",
		DomainName:      "disabled.cloudfront.localhost",
		Aliases:         []string{"disabled.example.test"},
		Enabled:         false, // disabled
		Origins:         []*config.Origin{origin},
		DefaultBehavior: beh,
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	req := httptest.NewRequest(http.MethodGet, "http://disabled.example.test/", nil)
	req.Host = "disabled.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("disabled distribution: status=%d, want 403", rr.Code)
	}
}

// ------------------------------------------------------------------
// 2. Proxying to a custom origin
// ------------------------------------------------------------------

func TestProxy_StatusAndBodyPassThrough(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"200 OK", http.StatusOK, "hello world"},
		{"418 I'm a Teapot", http.StatusTeapot, "teapot body"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_ = originSrv

			origin := baseOrigin("o1", host, port)
			dist := baseDistribution("D1", "d1.cloudfront.localhost", []string{"assets.example.test"}, origin)
			cfg := &config.Config{Distributions: []*config.Distribution{dist}}
			srv := dataplane.New(cfg, newLogger())

			req := httptest.NewRequest(http.MethodGet, "http://assets.example.test/path", nil)
			req.Host = "assets.example.test"
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)

			if rr.Code != tc.status {
				t.Errorf("status=%d want=%d", rr.Code, tc.status)
			}
			if rr.Body.String() != tc.body {
				t.Errorf("body=%q want=%q", rr.Body.String(), tc.body)
			}
		})
	}
}

func TestProxy_OriginPath(t *testing.T) {
	var receivedPath string
	var receivedRawQuery string

	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedRawQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	origin := &config.Origin{
		ID:         "o1",
		OriginPath: "/base",
		Custom: &config.CustomOrigin{
			Host:             host,
			HTTPPort:         port,
			ProtocolPolicy:   "http-only",
			ReadTimeout:      5 * time.Second,
			KeepaliveTimeout: 5 * time.Second,
		},
	}
	dist := baseDistribution("D1", "d1.cloudfront.localhost", []string{"assets.example.test"}, origin)
	// Forward all query strings to the origin (CachingOptimized would strip them).
	dist.DefaultBehavior.CachePolicy.QueryStrings = config.ListSelection{Behavior: "all"}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	req := httptest.NewRequest(http.MethodGet, "http://assets.example.test/file.txt?foo=bar", nil)
	req.Host = "assets.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status=%d want=200", rr.Code)
	}
	if receivedPath != "/base/file.txt" {
		t.Errorf("origin path=%q want=/base/file.txt", receivedPath)
	}
	if receivedRawQuery != "foo=bar" {
		t.Errorf("query=%q want=foo=bar", receivedRawQuery)
	}
}

func TestProxy_DefaultRootObject(t *testing.T) {
	var receivedPath string
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	origin := baseOrigin("o1", host, port)
	dist := baseDistribution("D1", "d1.cloudfront.localhost", []string{"assets.example.test"}, origin)
	dist.DefaultRootObject = "index.html"
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	do := func(path string) string {
		req := httptest.NewRequest(http.MethodGet, "http://assets.example.test"+path, nil)
		req.Host = "assets.example.test"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("path %q: status=%d want=200", path, rr.Code)
		}
		return receivedPath
	}

	// CloudFront applies the default root object at the distribution root for
	// every origin type, including custom origins.
	if got := do("/"); got != "/index.html" {
		t.Errorf("custom origin received %q for '/', want /index.html", got)
	}
	// Subdirectory paths are left untouched.
	if got := do("/sub/page.html"); got != "/sub/page.html" {
		t.Errorf("custom origin received %q, want /sub/page.html (subdir unchanged)", got)
	}
}

func TestProxy_OriginCustomHeaders(t *testing.T) {
	var gotCustom, gotViewer string

	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotCustom = r.Header.Get("X-Custom-Auth")
		gotViewer = r.Header.Get("X-Viewer-Header")
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	origin := &config.Origin{
		ID: "o1",
		Custom: &config.CustomOrigin{
			Host:             host,
			HTTPPort:         port,
			ProtocolPolicy:   "http-only",
			ReadTimeout:      5 * time.Second,
			KeepaliveTimeout: 5 * time.Second,
		},
		CustomHeaders: []config.Header{
			{Name: "X-Custom-Auth", Value: "secret-token"},
			// X-Viewer-Header is also set as a custom header, should override viewer value
			{Name: "X-Viewer-Header", Value: "from-origin-config"},
		},
	}
	dist := baseDistribution("D1", "d1.cloudfront.localhost", []string{"assets.example.test"}, origin)
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	req := httptest.NewRequest(http.MethodGet, "http://assets.example.test/", nil)
	req.Host = "assets.example.test"
	req.Header.Set("X-Viewer-Header", "from-viewer")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status=%d want=200", rr.Code)
	}
	if gotCustom != "secret-token" {
		t.Errorf("X-Custom-Auth=%q want=secret-token", gotCustom)
	}
	if gotViewer != "from-origin-config" {
		t.Errorf("X-Viewer-Header=%q want=from-origin-config (custom header should override viewer)", gotViewer)
	}
}

// A gzip-encoded origin response must be forwarded verbatim. The custom-origin
// transport disables Go's implicit gzip, so it must neither inject
// Accept-Encoding: gzip (the policy did not select it) nor decompress the body.
func TestProxy_GzipResponseForwardedVerbatim(t *testing.T) {
	var sawAcceptEncoding string
	var gzipped bytes.Buffer
	gw := gzip.NewWriter(&gzipped)
	_, _ = io.WriteString(gw, "compressed origin body")
	_ = gw.Close()
	stored := gzipped.Bytes()

	_, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		sawAcceptEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(stored)
	})

	origin := baseOrigin("o1", host, port)
	dist := baseDistribution("D1", "d1.cloudfront.localhost", []string{"assets.example.test"}, origin)
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	req := httptest.NewRequest(http.MethodGet, "http://assets.example.test/", nil)
	req.Host = "assets.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want=200", rr.Code)
	}
	if sawAcceptEncoding != "" {
		t.Errorf("origin saw Accept-Encoding %q, want empty (policy did not forward it; transport must not inject gzip)", sawAcceptEncoding)
	}
	if ce := rr.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Errorf("Content-Encoding=%q want=gzip (origin response must pass through)", ce)
	}
	if !bytes.Equal(rr.Body.Bytes(), stored) {
		t.Errorf("body was altered: got %d bytes, want the %d gzip bytes the origin sent", rr.Body.Len(), len(stored))
	}
}

func TestProxy_HostHeaderReplacedByOriginHost(t *testing.T) {
	var receivedHost string

	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		receivedHost = r.Host
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	origin := baseOrigin("o1", host, port)
	dist := baseDistribution("D1", "d1.cloudfront.localhost", []string{"assets.example.test"}, origin)
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	req := httptest.NewRequest(http.MethodGet, "http://assets.example.test/", nil)
	req.Host = "assets.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status=%d want=200", rr.Code)
	}
	// origin should see its own host (127.0.0.1), not the alias
	if receivedHost == "assets.example.test" {
		t.Errorf("origin received viewer Host=%q, should be origin host, not alias", receivedHost)
	}
	if !strings.HasPrefix(receivedHost, host) {
		t.Errorf("origin host=%q want prefix %q", receivedHost, host)
	}
}

func TestProxy_XForwardedFor(t *testing.T) {
	var receivedXFF string

	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		receivedXFF = r.Header.Get("X-Forwarded-For")
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	origin := baseOrigin("o1", host, port)
	dist := baseDistribution("D1", "d1.cloudfront.localhost", []string{"assets.example.test"}, origin)
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	req := httptest.NewRequest(http.MethodGet, "http://assets.example.test/", nil)
	req.Host = "assets.example.test"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	// httptest.NewRequest sets RemoteAddr to "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status=%d want=200", rr.Code)
	}
	if !strings.HasPrefix(receivedXFF, "1.2.3.4,") {
		t.Errorf("X-Forwarded-For=%q want prefix '1.2.3.4,'", receivedXFF)
	}
}

func TestProxy_ViaHeader(t *testing.T) {
	var originVia string

	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		originVia = r.Header.Get("Via")
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	origin := baseOrigin("o1", host, port)
	domainName := "d1.cloudfront.localhost"
	dist := baseDistribution("D1", domainName, []string{"assets.example.test"}, origin)
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	req := httptest.NewRequest(http.MethodGet, "http://assets.example.test/", nil)
	req.Host = "assets.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	expectedVia := "1.1 " + domainName + " (CloudFront)"

	if rr.Code != http.StatusOK {
		t.Errorf("status=%d want=200", rr.Code)
	}
	if !strings.Contains(originVia, expectedVia) {
		t.Errorf("origin Via=%q want contains %q", originVia, expectedVia)
	}
	if !strings.Contains(rr.Header().Get("Via"), expectedVia) {
		t.Errorf("response Via=%q want contains %q", rr.Header().Get("Via"), expectedVia)
	}
}

func TestProxy_CFHeaders(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Validate X-Amz-Cf-Id is present on origin request
		cfID := r.Header.Get("X-Amz-Cf-Id")
		if len(cfID) != 56 {
			w.Header().Set("X-Test-Error", "bad X-Amz-Cf-Id length on origin request")
		}
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	origin := baseOrigin("o1", host, port)
	dist := baseDistribution("D1", "d1.cloudfront.localhost", []string{"assets.example.test"}, origin)
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	req := httptest.NewRequest(http.MethodGet, "http://assets.example.test/", nil)
	req.Host = "assets.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status=%d want=200", rr.Code)
	}
	if errMsg := rr.Header().Get("X-Test-Error"); errMsg != "" {
		t.Errorf("origin request error: %s", errMsg)
	}
	cfID := rr.Header().Get("X-Amz-Cf-Id")
	if len(cfID) != 56 {
		t.Errorf("response X-Amz-Cf-Id length=%d want=56, value=%q", len(cfID), cfID)
	}
	if got := rr.Header().Get("X-Cache"); got != "Miss from localfront" {
		t.Errorf("X-Cache=%q want 'Miss from localfront'", got)
	}
	if got := rr.Header().Get("X-Amz-Cf-Pop"); got == "" {
		t.Errorf("X-Amz-Cf-Pop missing from response")
	}
}

func TestProxy_HopByHopHeadersNotForwarded(t *testing.T) {
	var receivedConnection, receivedKeepAlive, receivedLinkedHeader string

	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		receivedConnection = r.Header.Get("Connection")
		receivedKeepAlive = r.Header.Get("Keep-Alive")
		receivedLinkedHeader = r.Header.Get("X-My-Conn-Header")
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	origin := baseOrigin("o1", host, port)
	dist := baseDistribution("D1", "d1.cloudfront.localhost", []string{"assets.example.test"}, origin)
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	req := httptest.NewRequest(http.MethodGet, "http://assets.example.test/", nil)
	req.Host = "assets.example.test"
	// Connection header listing X-My-Conn-Header as a connection-specific header
	req.Header.Set("Connection", "keep-alive, X-My-Conn-Header")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("X-My-Conn-Header", "some-value")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status=%d want=200", rr.Code)
	}
	if receivedConnection != "" {
		t.Errorf("Connection header forwarded to origin: %q", receivedConnection)
	}
	if receivedKeepAlive != "" {
		t.Errorf("Keep-Alive header forwarded to origin: %q", receivedKeepAlive)
	}
	if receivedLinkedHeader != "" {
		t.Errorf("Connection-listed header X-My-Conn-Header forwarded to origin: %q", receivedLinkedHeader)
	}
}

func TestProxy_POSTBodyReachesOrigin(t *testing.T) {
	var receivedBody []byte

	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		var err error
		receivedBody, err = io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	origin := baseOrigin("o1", host, port)
	beh := &config.Behavior{
		Origin:         origin,
		AllowedMethods: []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		CachePolicy: &config.CachePolicy{
			MinTTL:     0,
			DefaultTTL: 0,
			MaxTTL:     0,
		},
	}
	dist := &config.Distribution{
		LogicalID:       "D1",
		ID:              "D1",
		DomainName:      "d1.cloudfront.localhost",
		Aliases:         []string{"assets.example.test"},
		Enabled:         true,
		Origins:         []*config.Origin{origin},
		DefaultBehavior: beh,
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	body := "hello post body"
	req := httptest.NewRequest(http.MethodPost, "http://assets.example.test/submit", strings.NewReader(body))
	req.Host = "assets.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status=%d want=200", rr.Code)
	}
	if string(receivedBody) != body {
		t.Errorf("body=%q want=%q", string(receivedBody), body)
	}
}

// ------------------------------------------------------------------
// 3. Method not allowed
// ------------------------------------------------------------------

func TestMethodNotAllowed(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	origin := baseOrigin("o1", host, port)
	dist := baseDistribution("D1", "d1.cloudfront.localhost", []string{"assets.example.test"}, origin)
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	req := httptest.NewRequest(http.MethodDelete, "http://assets.example.test/resource", nil)
	req.Host = "assets.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("DELETE with GET/HEAD-only distribution: status=%d want=403", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "could not be satisfied") {
		t.Errorf("CF error page body missing 'could not be satisfied': %q", body)
	}
}

// ------------------------------------------------------------------
// 4. Origin connection failure → 502
// ------------------------------------------------------------------

func TestOriginConnectionFailure(t *testing.T) {
	// Start a listener, record its address, then close it immediately
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	h, p, _ := net.SplitHostPort(addr)
	port := parsePort(p)

	origin := baseOrigin("o1", h, port)
	dist := baseDistribution("D1", "d1.cloudfront.localhost", []string{"assets.example.test"}, origin)
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	req := httptest.NewRequest(http.MethodGet, "http://assets.example.test/", nil)
	req.Host = "assets.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("connection failure: status=%d want=502", rr.Code)
	}
	if got := rr.Header().Get("X-Cache"); got != "Error from localfront" {
		t.Errorf("X-Cache=%q want 'Error from localfront'", got)
	}
}

// ------------------------------------------------------------------
// 5. S3 origin → 502 with S3 message
// ------------------------------------------------------------------

func TestS3OriginNotImplemented(t *testing.T) {
	s3Origin := &config.Origin{
		ID: "s3-origin",
		S3: &config.S3Origin{
			Bucket: "my-bucket",
			Region: "us-east-1",
		},
	}
	beh := &config.Behavior{
		Origin:         s3Origin,
		AllowedMethods: []string{"GET", "HEAD"},
		CachePolicy:    &config.CachePolicy{},
	}
	dist := &config.Distribution{
		LogicalID:       "D1",
		ID:              "D1",
		DomainName:      "d1.cloudfront.localhost",
		Aliases:         []string{"assets.example.test"},
		Enabled:         true,
		Origins:         []*config.Origin{s3Origin},
		DefaultBehavior: beh,
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	srv := dataplane.New(cfg, newLogger())

	req := httptest.NewRequest(http.MethodGet, "http://assets.example.test/file.txt", nil)
	req.Host = "assets.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("S3 origin: status=%d want=502", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(strings.ToLower(body), "s3") {
		t.Errorf("S3 error page body does not mention S3: %q", body)
	}
}

// ------------------------------------------------------------------
// 6. SwapConfig: old alias 403s, new alias works, no restart
// ------------------------------------------------------------------

func TestSwapConfig(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	origin := baseOrigin("o1", host, port)

	// Config 1: alias is "old.example.test"
	dist1 := baseDistribution("D1", "d1.cloudfront.localhost", []string{"old.example.test"}, origin)
	cfg1 := &config.Config{Distributions: []*config.Distribution{dist1}}

	srv := dataplane.New(cfg1, newLogger())

	// old alias works before swap
	req := httptest.NewRequest(http.MethodGet, "http://old.example.test/", nil)
	req.Host = "old.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("before swap: old.example.test status=%d want=200", rr.Code)
	}

	// Config 2: alias is "new.example.test"
	dist2 := baseDistribution("D2", "d2.cloudfront.localhost", []string{"new.example.test"}, origin)
	cfg2 := &config.Config{Distributions: []*config.Distribution{dist2}}
	srv.SwapConfig(cfg2)

	// old alias should now 403
	req2 := httptest.NewRequest(http.MethodGet, "http://old.example.test/", nil)
	req2.Host = "old.example.test"
	rr2 := httptest.NewRecorder()
	srv.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusForbidden {
		t.Errorf("after swap: old.example.test status=%d want=403", rr2.Code)
	}

	// new alias should work
	req3 := httptest.NewRequest(http.MethodGet, "http://new.example.test/", nil)
	req3.Host = "new.example.test"
	rr3 := httptest.NewRecorder()
	srv.ServeHTTP(rr3, req3)
	if rr3.Code != http.StatusOK {
		t.Errorf("after swap: new.example.test status=%d want=200", rr3.Code)
	}
}
