package behavior

import (
	"net/http"
	"testing"

	"github.com/mackee/localfront/internal/config"
)

func TestApplyResponseHeaders_SecurityAndCustom(t *testing.T) {
	b := &config.Behavior{ResponseHeadersPolicy: &config.ResponseHeadersPolicy{
		CustomHeaders: []config.CustomHeader{
			{Name: "X-Always", Value: "set", Override: true},
			{Name: "X-Keep-Origin", Value: "policy", Override: false},
		},
		RemoveHeaders: []string{"X-Powered-By"},
		Security: &config.SecurityHeaders{
			ContentTypeOptions:      &config.HeaderToggle{Override: true},
			FrameOptions:            &config.HeaderValue{Value: "SAMEORIGIN", Override: false},
			StrictTransportSecurity: &config.HSTS{MaxAgeSec: 31536000, IncludeSubdomains: true},
			XSSProtection:           &config.XSSProtection{Protection: true, ModeBlock: true},
		},
	}}

	resp := http.Header{}
	resp.Set("X-Powered-By", "origin")
	resp.Set("X-Keep-Origin", "origin")
	ApplyResponseHeaders(b, resp, "")

	if resp.Get("X-Powered-By") != "" {
		t.Errorf("X-Powered-By should be removed")
	}
	if got := resp.Get("X-Always"); got != "set" {
		t.Errorf("X-Always = %q, want set", got)
	}
	if got := resp.Get("X-Keep-Origin"); got != "origin" {
		t.Errorf("X-Keep-Origin = %q, want origin (Override=false keeps origin value)", got)
	}
	if got := resp.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := resp.Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Errorf("X-Frame-Options = %q, want SAMEORIGIN", got)
	}
	if got := resp.Get("Strict-Transport-Security"); got != "max-age=31536000; includeSubDomains" {
		t.Errorf("Strict-Transport-Security = %q", got)
	}
	if got := resp.Get("X-XSS-Protection"); got != "1; mode=block" {
		t.Errorf("X-XSS-Protection = %q, want '1; mode=block'", got)
	}
}

func TestApplyResponseHeaders_SecurityOverrideFalseKeepsOrigin(t *testing.T) {
	b := &config.Behavior{ResponseHeadersPolicy: &config.ResponseHeadersPolicy{
		Security: &config.SecurityHeaders{
			FrameOptions: &config.HeaderValue{Value: "SAMEORIGIN", Override: false},
		},
	}}
	resp := http.Header{}
	resp.Set("X-Frame-Options", "DENY") // origin already set it
	ApplyResponseHeaders(b, resp, "")
	if got := resp.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY (origin value kept when Override=false)", got)
	}
}

func TestApplyCors_Wildcard(t *testing.T) {
	b := &config.Behavior{ResponseHeadersPolicy: &config.ResponseHeadersPolicy{
		Cors: &config.CorsConfig{
			AllowOrigins: []string{"*"},
			AllowMethods: []string{"GET", "HEAD", "OPTIONS"},
			AllowHeaders: []string{"*"},
			HasMaxAge:    true,
			MaxAgeSec:    600,
		},
	}}
	resp := http.Header{}
	ApplyResponseHeaders(b, resp, "https://app.example.test")
	if got := resp.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := resp.Get("Access-Control-Allow-Methods"); got != "GET, HEAD, OPTIONS" {
		t.Errorf("Access-Control-Allow-Methods = %q", got)
	}
	if got := resp.Get("Access-Control-Max-Age"); got != "600" {
		t.Errorf("Access-Control-Max-Age = %q, want 600", got)
	}
}

func TestApplyCors_EchoOriginAndVary(t *testing.T) {
	b := &config.Behavior{ResponseHeadersPolicy: &config.ResponseHeadersPolicy{
		Cors: &config.CorsConfig{
			AllowOrigins:     []string{"https://app.example.test", "https://admin.example.test"},
			AllowCredentials: true,
		},
	}}
	resp := http.Header{}
	ApplyResponseHeaders(b, resp, "https://admin.example.test")
	if got := resp.Get("Access-Control-Allow-Origin"); got != "https://admin.example.test" {
		t.Errorf("Access-Control-Allow-Origin = %q, want echoed origin", got)
	}
	if got := resp.Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want Origin", got)
	}
	if got := resp.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want true", got)
	}
}

func TestApplyCors_NotAddedWithoutOriginHeader(t *testing.T) {
	b := &config.Behavior{ResponseHeadersPolicy: &config.ResponseHeadersPolicy{
		Cors: &config.CorsConfig{AllowOrigins: []string{"*"}},
	}}
	resp := http.Header{}
	ApplyResponseHeaders(b, resp, "") // not a CORS request
	if resp.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("CORS headers should not be added when the request has no Origin")
	}
}

func TestApplyCors_OriginOverride(t *testing.T) {
	resp := http.Header{}
	resp.Set("Access-Control-Allow-Origin", "https://origin-set.example.test")

	noOverride := &config.Behavior{ResponseHeadersPolicy: &config.ResponseHeadersPolicy{
		Cors: &config.CorsConfig{AllowOrigins: []string{"*"}, OriginOverride: false},
	}}
	ApplyResponseHeaders(noOverride, resp, "https://app.example.test")
	if got := resp.Get("Access-Control-Allow-Origin"); got != "https://origin-set.example.test" {
		t.Errorf("with OriginOverride=false the origin value should win, got %q", got)
	}

	resp2 := http.Header{}
	resp2.Set("Access-Control-Allow-Origin", "https://origin-set.example.test")
	override := &config.Behavior{ResponseHeadersPolicy: &config.ResponseHeadersPolicy{
		Cors: &config.CorsConfig{AllowOrigins: []string{"*"}, OriginOverride: true},
	}}
	ApplyResponseHeaders(override, resp2, "https://app.example.test")
	if got := resp2.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("with OriginOverride=true the policy value should win, got %q", got)
	}
}
