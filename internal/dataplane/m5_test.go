package dataplane_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mackee/localfront/internal/cffunc"
	"github.com/mackee/localfront/internal/config"
	"github.com/mackee/localfront/internal/dataplane"
)

func mustCompileFn(t *testing.T, name, code string, kvs *cffunc.KVS) *cffunc.Function {
	t.Helper()
	f, err := cffunc.Compile(cffunc.Options{Name: name, Code: code, KVS: kvs})
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	t.Cleanup(f.Close)
	return f
}

// viewer-request function rewrites the URI; the origin receives the new path.
func TestM5_ViewerRequest_URIRewrite(t *testing.T) {
	var gotPath string
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	o := customOrigin("o1", host, port)
	fnCfg := &config.Function{LogicalID: "Rewrite", Name: "rewrite"}
	beh := getHeadBehavior(o, "")
	beh.ViewerRequest = fnCfg
	dist := &config.Distribution{
		LogicalID: "D1", ID: "D1", DomainName: "d1.cloudfront.localhost",
		Aliases: []string{"site.example.test"}, Enabled: true,
		Origins: []*config.Origin{o}, DefaultBehavior: beh,
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}

	fn := mustCompileFn(t, "rewrite", `
function handler(event) {
	event.request.uri = '/rewritten' + event.request.uri;
	return event.request;
}`, nil)
	srv := dataplane.New(cfg, newLogger())
	srv.Swap(cfg, map[string]*cffunc.Function{"Rewrite": fn})

	req := httptest.NewRequest(http.MethodGet, "http://site.example.test/page", nil)
	req.Host = "site.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if gotPath != "/rewritten/page" {
		t.Errorf("origin received path %q, want /rewritten/page", gotPath)
	}
}

// viewer-request function returns a redirect; the origin is never contacted.
func TestM5_ViewerRequest_Redirect(t *testing.T) {
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
	srv := dataplane.New(cfg, newLogger())
	srv.Swap(cfg, map[string]*cffunc.Function{"Redir": fn})

	req := httptest.NewRequest(http.MethodGet, "http://site.example.test/old", nil)
	req.Host = "site.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusMovedPermanently {
		t.Errorf("status = %d, want 301", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "https://new.example.test/" {
		t.Errorf("Location = %q", got)
	}
	if originHit {
		t.Error("origin should not be contacted for a function short-circuit")
	}
}

// viewer-response function adds a header to the origin response.
func TestM5_ViewerResponse_AddHeader(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "body")
	})
	_ = originSrv

	o := customOrigin("o1", host, port)
	beh := getHeadBehavior(o, "")
	beh.ViewerResponse = &config.Function{LogicalID: "Resp", Name: "resp"}
	dist := &config.Distribution{
		LogicalID: "D1", ID: "D1", DomainName: "d1.cloudfront.localhost",
		Aliases: []string{"site.example.test"}, Enabled: true,
		Origins: []*config.Origin{o}, DefaultBehavior: beh,
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}

	fn := mustCompileFn(t, "resp", `
function handler(event) {
	event.response.headers['x-frame-options'] = { value: 'DENY' };
	return event.response;
}`, nil)
	srv := dataplane.New(cfg, newLogger())
	srv.Swap(cfg, map[string]*cffunc.Function{"Resp": fn})

	req := httptest.NewRequest(http.MethodGet, "http://site.example.test/", nil)
	req.Host = "site.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY (added by viewer-response function)", got)
	}
	if body := rr.Body.String(); body != "body" {
		t.Errorf("body = %q, want body", body)
	}
}

// CloudFront Functions are not invoked for origin responses with a 4xx/5xx
// status, so the viewer-response function must not run on an origin error.
func TestM5_ViewerResponse_SkippedOnOriginError(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "not found")
	})
	_ = originSrv

	o := customOrigin("o1", host, port)
	beh := getHeadBehavior(o, "")
	beh.ViewerResponse = &config.Function{LogicalID: "Resp", Name: "resp"}
	dist := &config.Distribution{
		LogicalID: "D1", ID: "D1", DomainName: "d1.cloudfront.localhost",
		Aliases: []string{"site.example.test"}, Enabled: true,
		Origins: []*config.Origin{o}, DefaultBehavior: beh,
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}

	fn := mustCompileFn(t, "resp", `
function handler(event) {
	event.response.headers['x-frame-options'] = { value: 'DENY' };
	return event.response;
}`, nil)
	srv := dataplane.New(cfg, newLogger())
	srv.Swap(cfg, map[string]*cffunc.Function{"Resp": fn})

	req := httptest.NewRequest(http.MethodGet, "http://site.example.test/missing", nil)
	req.Host = "site.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if got := rr.Header().Get("X-Frame-Options"); got != "" {
		t.Errorf("X-Frame-Options = %q, want empty (function must not run on a 4xx origin response)", got)
	}
}

// A custom error response that rewrites the origin error below 400 makes the
// response eligible for the viewer-response function again.
func TestM5_ViewerResponse_RunsAfterErrorResponseRewrite(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "not found")
	})
	_ = originSrv

	o := customOrigin("o1", host, port)
	beh := getHeadBehavior(o, "")
	beh.ViewerResponse = &config.Function{LogicalID: "Resp", Name: "resp"}
	dist := &config.Distribution{
		LogicalID: "D1", ID: "D1", DomainName: "d1.cloudfront.localhost",
		Aliases: []string{"site.example.test"}, Enabled: true,
		Origins: []*config.Origin{o}, DefaultBehavior: beh,
		ErrorResponses: []*config.ErrorResponse{
			{ErrorCode: 404, ResponseCode: 200, ResponsePagePath: ""},
		},
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}

	fn := mustCompileFn(t, "resp", `
function handler(event) {
	event.response.headers['x-frame-options'] = { value: 'DENY' };
	return event.response;
}`, nil)
	srv := dataplane.New(cfg, newLogger())
	srv.Swap(cfg, map[string]*cffunc.Function{"Resp": fn})

	req := httptest.NewRequest(http.MethodGet, "http://site.example.test/missing", nil)
	req.Host = "site.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (custom error response rewrote the 404)", rr.Code)
	}
	if got := rr.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY (function should run after the rewrite to 200)", got)
	}
}

// The behavior is selected once, before the function runs: a function rewriting
// the URI does NOT cause re-selection to a different behavior/origin.
func TestM5_BehaviorNotReevaluatedAfterRewrite(t *testing.T) {
	var defaultPath, apiPath string
	defaultSrv, dh, dp := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		defaultPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	_ = defaultSrv
	apiSrv, ah, ap := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		apiPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	_ = apiSrv

	defaultOrigin := customOrigin("default", dh, dp)
	apiOrigin := customOrigin("api", ah, ap)

	defaultBeh := getHeadBehavior(defaultOrigin, "")
	defaultBeh.ViewerRequest = &config.Function{LogicalID: "ToApi", Name: "toapi"}

	dist := &config.Distribution{
		LogicalID: "D1", ID: "D1", DomainName: "d1.cloudfront.localhost",
		Aliases: []string{"site.example.test"}, Enabled: true,
		Origins:         []*config.Origin{defaultOrigin, apiOrigin},
		DefaultBehavior: defaultBeh,
		Behaviors:       []*config.Behavior{getHeadBehavior(apiOrigin, "/api/*")},
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}

	fn := mustCompileFn(t, "toapi", `
function handler(event) {
	event.request.uri = '/api' + event.request.uri;
	return event.request;
}`, nil)
	srv := dataplane.New(cfg, newLogger())
	srv.Swap(cfg, map[string]*cffunc.Function{"ToApi": fn})

	req := httptest.NewRequest(http.MethodGet, "http://site.example.test/page", nil)
	req.Host = "site.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if defaultPath != "/api/page" {
		t.Errorf("default origin received %q, want /api/page (behavior must not be re-evaluated)", defaultPath)
	}
	if apiPath != "" {
		t.Errorf("api origin should not be hit, but received %q", apiPath)
	}
}

// A function runtime error becomes a CloudFront-compatible 503.
func TestM5_FunctionError503(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	o := customOrigin("o1", host, port)
	beh := getHeadBehavior(o, "")
	beh.ViewerRequest = &config.Function{LogicalID: "Boom", Name: "boom"}
	dist := &config.Distribution{
		LogicalID: "D1", ID: "D1", DomainName: "d1.cloudfront.localhost",
		Aliases: []string{"site.example.test"}, Enabled: true,
		Origins: []*config.Origin{o}, DefaultBehavior: beh,
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}

	fn := mustCompileFn(t, "boom", `function handler(event) { throw new Error('boom'); }`, nil)
	srv := dataplane.New(cfg, newLogger())
	srv.Swap(cfg, map[string]*cffunc.Function{"Boom": fn})

	req := httptest.NewRequest(http.MethodGet, "http://site.example.test/", nil)
	req.Host = "site.example.test"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

// KVS feature flag changes behavior; re-seeding changes the outcome.
func TestM5_KVSFeatureFlag(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Variant", r.Header.Get("X-Variant"))
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	o := customOrigin("o1", host, port)
	beh := getHeadBehavior(o, "")
	beh.ViewerRequest = &config.Function{LogicalID: "Flag", Name: "flag"}
	// Forward the function-added header to the origin.
	beh.OriginRequestPolicy = &config.OriginRequestPolicy{Headers: config.ListSelection{Behavior: "allViewer"}}
	dist := &config.Distribution{
		LogicalID: "D1", ID: "D1", DomainName: "d1.cloudfront.localhost",
		Aliases: []string{"site.example.test"}, Enabled: true,
		Origins: []*config.Origin{o}, DefaultBehavior: beh,
	}
	cfg := &config.Config{Distributions: []*config.Distribution{dist}}

	store := cffunc.NewKVS()
	store.Replace(map[string]string{"variant": "A"})
	fn := mustCompileFn(t, "flag", `
import cf from 'cloudfront';
const kvs = cf.kvs();
async function handler(event) {
	const v = await kvs.get('variant');
	event.request.headers['x-variant'] = { value: v };
	return event.request;
}`, store)
	srv := dataplane.New(cfg, newLogger())
	srv.Swap(cfg, map[string]*cffunc.Function{"Flag": fn})

	do := func() string {
		req := httptest.NewRequest(http.MethodGet, "http://site.example.test/", nil)
		req.Host = "site.example.test"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr.Header().Get("X-Variant")
	}

	if got := do(); got != "A" {
		t.Errorf("variant = %q, want A", got)
	}
	store.Replace(map[string]string{"variant": "B"}) // re-seed
	if got := do(); got != "B" {
		t.Errorf("variant after re-seed = %q, want B", got)
	}
}
