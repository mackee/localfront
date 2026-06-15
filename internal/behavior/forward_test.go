package behavior

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/mackee/localfront/internal/config"
)

func sel(behavior string, items ...string) config.ListSelection {
	return config.ListSelection{Behavior: behavior, Items: items}
}

// newViewerRequest builds a request with the given header/cookie/query inputs.
func newViewerRequest(query string) *http.Request {
	target := "/path"
	if query != "" {
		target += "?" + query
	}
	r := httptest.NewRequest(http.MethodGet, "http://cdn.example.test"+target, nil)
	r.Host = "cdn.example.test"
	return r
}

func sortedHeaderNames(h http.Header) []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestBuildOriginRequest_Headers(t *testing.T) {
	viewer := http.Header{"Cloudfront-Viewer-Country": {"JP"}}

	tests := []struct {
		name        string
		cache       *config.CachePolicy
		orp         *config.OriginRequestPolicy
		reqHeaders  map[string]string
		wantForward []string // header names expected to reach the origin
		wantAbsent  []string // header names expected to be dropped
	}{
		{
			name:        "CachingOptimized forwards no viewer headers",
			cache:       &config.CachePolicy{Headers: sel("none")},
			reqHeaders:  map[string]string{"X-Custom": "v", "Authorization": "bearer"},
			wantForward: nil,
			wantAbsent:  []string{"X-Custom", "Authorization"},
		},
		{
			name:        "cache whitelist forwards listed viewer headers",
			cache:       &config.CachePolicy{Headers: sel("whitelist", "Authorization")},
			reqHeaders:  map[string]string{"Authorization": "bearer", "X-Custom": "v"},
			wantForward: []string{"Authorization"},
			wantAbsent:  []string{"X-Custom"},
		},
		{
			// Legacy ForwardedValues.Headers: ['*'] maps to Behavior "all".
			name:        "legacy all forwards every viewer header",
			cache:       &config.CachePolicy{Headers: sel("all")},
			reqHeaders:  map[string]string{"X-Custom": "v", "Authorization": "bearer"},
			wantForward: []string{"X-Custom", "Authorization"},
		},
		{
			name:        "ORP allViewer forwards all viewer headers",
			cache:       &config.CachePolicy{Headers: sel("none")},
			orp:         &config.OriginRequestPolicy{Headers: sel("allViewer")},
			reqHeaders:  map[string]string{"X-Custom": "v", "Authorization": "bearer"},
			wantForward: []string{"X-Custom", "Authorization"},
		},
		{
			name:        "ORP allExcept forwards everything but the excepted header",
			orp:         &config.OriginRequestPolicy{Headers: sel("allExcept", "X-Secret")},
			reqHeaders:  map[string]string{"X-Custom": "v", "X-Secret": "s"},
			wantForward: []string{"X-Custom"},
			wantAbsent:  []string{"X-Secret"},
		},
		{
			name:        "union of cache and ORP whitelists",
			cache:       &config.CachePolicy{Headers: sel("whitelist", "Authorization")},
			orp:         &config.OriginRequestPolicy{Headers: sel("whitelist", "X-Custom")},
			reqHeaders:  map[string]string{"Authorization": "b", "X-Custom": "v", "X-Other": "o"},
			wantForward: []string{"Authorization", "X-Custom"},
			wantAbsent:  []string{"X-Other"},
		},
		{
			name:        "cache whitelist pulls synthesized CloudFront header",
			cache:       &config.CachePolicy{Headers: sel("whitelist", "CloudFront-Viewer-Country")},
			reqHeaders:  map[string]string{"X-Custom": "v"},
			wantForward: []string{"CloudFront-Viewer-Country"},
			wantAbsent:  []string{"X-Custom"},
		},
		{
			name: "allViewerAndWhitelistCloudFront adds listed CloudFront header",
			orp: &config.OriginRequestPolicy{
				Headers: sel("allViewerAndWhitelistCloudFront", "CloudFront-Viewer-Country"),
			},
			reqHeaders:  map[string]string{"X-Custom": "v"},
			wantForward: []string{"X-Custom", "CloudFront-Viewer-Country"},
		},
		{
			name:        "plain allViewer does not add CloudFront headers",
			orp:         &config.OriginRequestPolicy{Headers: sel("allViewer")},
			reqHeaders:  map[string]string{"X-Custom": "v"},
			wantForward: []string{"X-Custom"},
			wantAbsent:  []string{"CloudFront-Viewer-Country"},
		},
		{
			name:        "viewer-supplied CloudFront header is dropped",
			orp:         &config.OriginRequestPolicy{Headers: sel("allViewer")},
			reqHeaders:  map[string]string{"CloudFront-Viewer-Country": "FR", "X-Custom": "v"},
			wantForward: []string{"X-Custom"},
			wantAbsent:  []string{"CloudFront-Viewer-Country"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newViewerRequest("")
			for k, v := range tc.reqHeaders {
				r.Header.Set(k, v)
			}
			b := &config.Behavior{CachePolicy: tc.cache, OriginRequestPolicy: tc.orp}
			got := BuildOriginRequest(b, r, viewer)

			for _, name := range tc.wantForward {
				if got.Headers.Get(name) == "" {
					t.Errorf("header %q should be forwarded, got headers %v", name, sortedHeaderNames(got.Headers))
				}
			}
			for _, name := range tc.wantAbsent {
				if got.Headers.Get(name) != "" {
					t.Errorf("header %q should be dropped, got %q", name, got.Headers.Get(name))
				}
			}
		})
	}
}

func TestBuildOriginRequest_CloudFrontHeaderValueFromPool(t *testing.T) {
	viewer := http.Header{"Cloudfront-Viewer-Country": {"JP"}}
	b := &config.Behavior{
		CachePolicy: &config.CachePolicy{Headers: sel("whitelist", "CloudFront-Viewer-Country")},
	}
	r := newViewerRequest("")
	got := BuildOriginRequest(b, r, viewer)
	if v := got.Headers.Get("CloudFront-Viewer-Country"); v != "JP" {
		t.Errorf("CloudFront-Viewer-Country = %q, want JP (from synthesized pool)", v)
	}
}

func TestBuildOriginRequest_Cookies(t *testing.T) {
	tests := []struct {
		name   string
		cache  config.ListSelection
		orp    config.ListSelection
		cookie string
		want   string
	}{
		{"none drops all", sel("none"), config.ListSelection{}, "a=1; b=2", ""},
		{"all keeps all", sel("all"), config.ListSelection{}, "a=1; b=2", "a=1; b=2"},
		{"whitelist keeps listed", sel("whitelist", "b"), config.ListSelection{}, "a=1; b=2; c=3", "b=2"},
		{"allExcept drops listed", sel("allExcept", "a"), config.ListSelection{}, "a=1; b=2", "b=2"},
		{"union of cache and orp", sel("whitelist", "a"), sel("whitelist", "c"), "a=1; b=2; c=3", "a=1; c=3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := &config.Behavior{
				CachePolicy:         &config.CachePolicy{Cookies: tc.cache},
				OriginRequestPolicy: &config.OriginRequestPolicy{Cookies: tc.orp},
			}
			r := newViewerRequest("")
			r.Header.Set("Cookie", tc.cookie)
			got := BuildOriginRequest(b, r, http.Header{})
			if g := got.Headers.Get("Cookie"); g != tc.want {
				t.Errorf("Cookie = %q, want %q", g, tc.want)
			}
		})
	}
}

func TestBuildOriginRequest_QueryStrings(t *testing.T) {
	tests := []struct {
		name  string
		cache config.ListSelection
		orp   config.ListSelection
		query string
		want  string
	}{
		{"none drops all", sel("none"), config.ListSelection{}, "a=1&b=2", ""},
		{"all keeps order", sel("all"), config.ListSelection{}, "b=2&a=1", "b=2&a=1"},
		{"whitelist keeps listed in order", sel("whitelist", "a"), config.ListSelection{}, "a=1&b=2&a=3", "a=1&a=3"},
		{"allExcept drops listed", sel("allExcept", "token"), config.ListSelection{}, "token=x&page=2", "page=2"},
		{"union", sel("whitelist", "a"), sel("whitelist", "c"), "a=1&b=2&c=3", "a=1&c=3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := &config.Behavior{
				CachePolicy:         &config.CachePolicy{QueryStrings: tc.cache},
				OriginRequestPolicy: &config.OriginRequestPolicy{QueryStrings: tc.orp},
			}
			r := newViewerRequest(tc.query)
			got := BuildOriginRequest(b, r, http.Header{})
			if got.RawQuery != tc.want {
				t.Errorf("RawQuery = %q, want %q", got.RawQuery, tc.want)
			}
		})
	}
}

func TestBuildOriginRequest_ForwardHost(t *testing.T) {
	tests := []struct {
		name  string
		cache *config.CachePolicy
		orp   *config.OriginRequestPolicy
		want  bool
	}{
		{"none keeps origin host", &config.CachePolicy{Headers: sel("none")}, nil, false},
		{"cache whitelist Host", &config.CachePolicy{Headers: sel("whitelist", "Host")}, nil, true},
		{"orp allViewer forwards host", nil, &config.OriginRequestPolicy{Headers: sel("allViewer")}, true},
		{"orp allExcept Host strips host", nil, &config.OriginRequestPolicy{Headers: sel("allExcept", "Host")}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := &config.Behavior{CachePolicy: tc.cache, OriginRequestPolicy: tc.orp}
			got := BuildOriginRequest(b, newViewerRequest(""), http.Header{})
			if got.ForwardHost != tc.want {
				t.Errorf("ForwardHost = %v, want %v", got.ForwardHost, tc.want)
			}
		})
	}
}

func TestNormalizeAcceptEncoding(t *testing.T) {
	tests := []struct {
		name   string
		cache  *config.CachePolicy
		orp    *config.OriginRequestPolicy
		viewer string
		want   string
	}{
		{"both enabled, viewer both", &config.CachePolicy{Gzip: true, Brotli: true}, nil, "gzip, br, deflate", "gzip, br"},
		{"both enabled, viewer gzip only", &config.CachePolicy{Gzip: true, Brotli: true}, nil, "gzip", "gzip"},
		{"gzip enabled, viewer br only", &config.CachePolicy{Gzip: true}, nil, "br", ""},
		{"neither enabled, not whitelisted", &config.CachePolicy{Headers: sel("none")}, nil, "gzip", ""},
		{
			"neither enabled but Accept-Encoding whitelisted",
			&config.CachePolicy{Headers: sel("whitelist", "Accept-Encoding")},
			nil, "gzip, br", "gzip, br",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := &config.Behavior{CachePolicy: tc.cache, OriginRequestPolicy: tc.orp}
			r := newViewerRequest("")
			r.Header.Set("Accept-Encoding", tc.viewer)
			got := BuildOriginRequest(b, r, http.Header{})
			if got.AcceptEncoding != tc.want {
				t.Errorf("AcceptEncoding = %q, want %q", got.AcceptEncoding, tc.want)
			}
		})
	}
}

func TestBuildOriginRequest_DropsControlAndCloudFrontHeaders(t *testing.T) {
	b := &config.Behavior{OriginRequestPolicy: &config.OriginRequestPolicy{Headers: sel("allViewer")}}
	r := newViewerRequest("")
	r.Header.Set("X-Localfront-Viewer-Country", "JP")
	r.Header.Set("X-Real-Header", "keep")
	got := BuildOriginRequest(b, r, http.Header{})
	if got.Headers.Get("X-Localfront-Viewer-Country") != "" {
		t.Error("X-Localfront-* control headers must not reach the origin")
	}
	if !strings.EqualFold(got.Headers.Get("X-Real-Header"), "keep") {
		t.Error("ordinary viewer headers should still be forwarded under allViewer")
	}
}
