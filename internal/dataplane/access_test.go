package dataplane_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mackee/localfront/internal/accesslog"
	"github.com/mackee/localfront/internal/cffunc"
	"github.com/mackee/localfront/internal/config"
	"github.com/mackee/localfront/internal/dataplane"
)

// readLogLines drops the W3C-style header that NewWriter prepends and returns
// the per-request data lines a test can assert on.
func readLogLines(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	out := strings.TrimPrefix(buf.String(), accesslog.Header)
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func newAccessSrv(t *testing.T, dist *config.Distribution, buf *bytes.Buffer) *dataplane.Server {
	t.Helper()
	w, err := accesslog.NewWriter(buf)
	if err != nil {
		t.Fatalf("accesslog.NewWriter: %v", err)
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}
	return dataplane.New(cfg, newLogger(), dataplane.WithAccessLog(w))
}

// TestAccessLog_SuccessfulProxy covers the happy path: an origin replies with
// 200 + body, the log line records status, byte counts, and the result type.
func TestAccessLog_SuccessfulProxy(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello world"))
	})
	_ = originSrv

	origin := baseOrigin("o1", host, port)
	dist := baseDistribution("D1", "d1.cloudfront.localhost", []string{"assets.example.test"}, origin)

	var buf bytes.Buffer
	srv := newAccessSrv(t, dist, &buf)

	req := httptest.NewRequest(http.MethodGet, "http://assets.example.test/index.html?token=abc", nil)
	req.Host = "assets.example.test"
	req.Header.Set("User-Agent", "curl/8.0")
	req.RemoteAddr = "203.0.113.4:54321"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	lines := readLogLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d: %#v", len(lines), lines)
	}
	cols := strings.Split(lines[0], "\t")
	if got, want := len(cols), 33; got != want {
		t.Fatalf("expected 33 columns, got %d: %#v", got, cols)
	}
	checks := map[int]string{
		2:  "LOCAL50-C1",          // x-edge-location
		4:  "203.0.113.4",         // c-ip
		5:  "GET",                 // cs-method
		6:  "assets.example.test", // cs(Host)
		7:  "/index.html",         // cs-uri-stem
		8:  "200",                 // sc-status
		10: "curl/8.0",            // cs(User-Agent)
		11: "token=abc",           // cs-uri-query
		13: "Miss",                // x-edge-result-type
		15: "assets.example.test", // x-host-header
		16: "http",                // cs-protocol
		23: "HTTP/1.1",            // cs-protocol-version
		26: "54321",               // c-port
		28: "Miss",                // x-edge-detailed-result-type
		29: "text/plain",          // sc-content-type
	}
	for idx, want := range checks {
		if cols[idx] != want {
			t.Errorf("col %d: got %q, want %q", idx, cols[idx], want)
		}
	}
	if cols[3] == "0" {
		t.Errorf("sc-bytes should be non-zero, got %q", cols[3])
	}
	if cols[18] == "0.000" && cols[27] == "0.000" {
		t.Errorf("time-taken / TTFB both zero — capture broken")
	}
}

// TestAccessLog_UnknownHost emits an "Error" result-type log line even when no
// distribution matched.
func TestAccessLog_UnknownHost(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv
	origin := baseOrigin("o1", host, port)
	dist := baseDistribution("D1", "d1.cloudfront.localhost", []string{"assets.example.test"}, origin)

	var buf bytes.Buffer
	srv := newAccessSrv(t, dist, &buf)

	req := httptest.NewRequest(http.MethodGet, "http://no-match.example.test/", nil)
	req.Host = "no-match.example.test"
	req.RemoteAddr = "192.0.2.10:443"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	lines := readLogLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d: %#v", len(lines), lines)
	}
	cols := strings.Split(lines[0], "\t")
	if cols[6] != "no-match.example.test" {
		t.Errorf("cs(Host): got %q, want no-match.example.test", cols[6])
	}
	if cols[8] != "403" {
		t.Errorf("sc-status: got %q, want 403", cols[8])
	}
	if cols[13] != "Error" {
		t.Errorf("x-edge-result-type: got %q, want Error", cols[13])
	}
}

// TestAccessLog_QueryEncoding ensures CloudFront-style URL encoding handles
// spaces in the query string without breaking the tab separator.
func TestAccessLog_QueryEncoding(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv
	origin := baseOrigin("o1", host, port)
	dist := baseDistribution("D1", "d1.cloudfront.localhost", []string{"assets.example.test"}, origin)

	var buf bytes.Buffer
	srv := newAccessSrv(t, dist, &buf)

	req := httptest.NewRequest(http.MethodGet, "http://assets.example.test/path", nil)
	// Raw query with a space — set after NewRequest so its URL parser doesn't
	// reject the test fixture.
	req.URL.RawQuery = "q=hello world"
	req.Host = "assets.example.test"
	req.RemoteAddr = "203.0.113.5:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	lines := readLogLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(lines))
	}
	cols := strings.Split(lines[0], "\t")
	if got := cols[11]; !strings.Contains(got, "%20") {
		t.Errorf("cs-uri-query should encode space as %%20: got %q", got)
	}
}

// TestAccessLog_DisabledByDefault makes sure a server constructed without
// WithAccessLog writes nothing to any destination — the wrapping is purely
// opt-in.
func TestAccessLog_DisabledByDefault(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
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
	// Nothing observable here beyond a successful request — there is no log
	// sink to inspect because the option was never passed.
	_ = rr
}

// TestAccessLog_FunctionGenerated records the FunctionGeneratedResponse
// result type when a CloudFront Function short-circuits the pipeline with
// its own response — the origin is never contacted.
func TestAccessLog_FunctionGenerated(t *testing.T) {
	originHit := false
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		originHit = true
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv
	o := customOrigin("o1", host, port)
	beh := getHeadBehavior(o, "")
	beh.ViewerRequest = &config.Function{LogicalID: "Redir", Name: "redir"}
	dist := &config.Distribution{
		LogicalID: "D1", ID: "D1", DomainName: "d1.cloudfront.localhost",
		Aliases: []string{"site.example.test"}, Enabled: true,
		Origins: []*config.Origin{o}, DefaultBehavior: beh,
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}

	fn := mustCompileFn(t, "redir", `
function handler(event) {
	return { statusCode: 301, statusDescription: 'Moved', headers: { location: { value: 'https://new.example.test/' } } };
}`, nil)

	var buf bytes.Buffer
	w, err := accesslog.NewWriter(&buf)
	if err != nil {
		t.Fatalf("accesslog.NewWriter: %v", err)
	}
	srv := dataplane.New(cfg, newLogger(), dataplane.WithAccessLog(w))
	srv.Swap(cfg, map[string]*cffunc.Function{"Redir": fn})

	req := httptest.NewRequest(http.MethodGet, "http://site.example.test/old", nil)
	req.Host = "site.example.test"
	req.RemoteAddr = "203.0.113.7:1111"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if originHit {
		t.Fatalf("origin must not be reached when function short-circuits")
	}
	lines := readLogLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(lines))
	}
	cols := strings.Split(lines[0], "\t")
	for _, idx := range []int{13, 22, 28} { // result, response-result, detailed
		if cols[idx] != "FunctionGeneratedResponse" {
			t.Errorf("col %d: got %q, want FunctionGeneratedResponse", idx, cols[idx])
		}
	}
}

// TestAccessLog_3xxFromOriginIsMiss verifies a 3xx response forwarded from the
// origin records as "Miss" (not "Redirect" — real CloudFront reserves
// Redirect for CF-generated 3xx responses).
func TestAccessLog_3xxFromOriginIsMiss(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(http.StatusFound)
	})
	_ = originSrv
	origin := baseOrigin("o1", host, port)
	dist := baseDistribution("D1", "d1.cloudfront.localhost", []string{"assets.example.test"}, origin)

	var buf bytes.Buffer
	srv := newAccessSrv(t, dist, &buf)

	req := httptest.NewRequest(http.MethodGet, "http://assets.example.test/old", nil)
	req.Host = "assets.example.test"
	req.RemoteAddr = "203.0.113.8:2222"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}
	lines := readLogLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(lines))
	}
	cols := strings.Split(lines[0], "\t")
	if cols[13] != "Miss" {
		t.Errorf("x-edge-result-type: got %q, want Miss (origin 3xx maps to Miss)", cols[13])
	}
}

// TestAccessLog_PostRequestBytes confirms the request-body counter feeds the
// cs-bytes column when a viewer uploads a body.
func TestAccessLog_PostRequestBytes(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv
	origin := baseOrigin("o1", host, port)
	dist := baseDistribution("D1", "d1.cloudfront.localhost", []string{"assets.example.test"}, origin)
	dist.DefaultBehavior.AllowedMethods = []string{"GET", "HEAD", "POST"}

	var buf bytes.Buffer
	srv := newAccessSrv(t, dist, &buf)

	body := strings.NewReader("payload-bytes")
	req := httptest.NewRequest(http.MethodPost, "http://assets.example.test/upload", body)
	req.Host = "assets.example.test"
	req.ContentLength = int64(body.Len())
	req.RemoteAddr = "203.0.113.6:5000"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	lines := readLogLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(lines))
	}
	cols := strings.Split(lines[0], "\t")
	if cols[5] != "POST" {
		t.Errorf("cs-method: got %q, want POST", cols[5])
	}
	// cs-bytes should at least include the body length.
	if cols[17] == "0" {
		t.Errorf("cs-bytes should include request body length, got %q", cols[17])
	}
}
