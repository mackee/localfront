package behavior

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/mackee/localfront/internal/config"
)

// ApplyResponseHeaders mutates resp in place to apply a response headers
// policy: remove-headers, custom headers, security headers, and CORS headers.
// requestOrigin is the viewer request's Origin header (CORS headers are only
// added for CORS requests, i.e. when it is non-empty). It is a no-op when the
// behavior has no response headers policy.
func ApplyResponseHeaders(b *config.Behavior, resp http.Header, requestOrigin string) {
	policy := b.ResponseHeadersPolicy
	if policy == nil {
		return
	}
	for _, name := range policy.RemoveHeaders {
		resp.Del(name)
	}
	for _, ch := range policy.CustomHeaders {
		if ch.Override || resp.Get(ch.Name) == "" {
			resp.Set(ch.Name, ch.Value)
		}
	}
	applySecurityHeaders(policy.Security, resp)
	if requestOrigin != "" {
		applyCors(policy.Cors, resp, requestOrigin)
	}
}

func applySecurityHeaders(sec *config.SecurityHeaders, resp http.Header) {
	if sec == nil {
		return
	}
	setIf := func(name, value string, override bool) {
		if override || resp.Get(name) == "" {
			resp.Set(name, value)
		}
	}
	if v := sec.ContentTypeOptions; v != nil {
		setIf("X-Content-Type-Options", "nosniff", v.Override)
	}
	if v := sec.FrameOptions; v != nil {
		setIf("X-Frame-Options", v.Value, v.Override)
	}
	if v := sec.ReferrerPolicy; v != nil {
		setIf("Referrer-Policy", v.Value, v.Override)
	}
	if v := sec.ContentSecurityPolicy; v != nil {
		setIf("Content-Security-Policy", v.Value, v.Override)
	}
	if v := sec.StrictTransportSecurity; v != nil {
		setIf("Strict-Transport-Security", hstsValue(v), v.Override)
	}
	if v := sec.XSSProtection; v != nil {
		setIf("X-XSS-Protection", xssValue(v), v.Override)
	}
}

func hstsValue(h *config.HSTS) string {
	v := "max-age=" + strconv.FormatInt(h.MaxAgeSec, 10)
	if h.IncludeSubdomains {
		v += "; includeSubDomains"
	}
	if h.Preload {
		v += "; preload"
	}
	return v
}

func xssValue(x *config.XSSProtection) string {
	if !x.Protection {
		return "0"
	}
	v := "1"
	if x.ModeBlock {
		v += "; mode=block"
	}
	if x.ReportURI != "" {
		v += "; report=" + x.ReportURI
	}
	return v
}

// applyCors adds the Access-Control-* response headers. With OriginOverride the
// policy values replace any the origin set; otherwise the origin's values win.
func applyCors(cors *config.CorsConfig, resp http.Header, requestOrigin string) {
	if cors == nil {
		return
	}
	allowOrigin, vary := corsAllowOrigin(cors.AllowOrigins, requestOrigin)
	if allowOrigin != "" {
		setCors(resp, "Access-Control-Allow-Origin", allowOrigin, cors.OriginOverride)
		if vary {
			addVary(resp, "Origin")
		}
	}
	if len(cors.AllowMethods) > 0 {
		setCors(resp, "Access-Control-Allow-Methods", joinCors(cors.AllowMethods), cors.OriginOverride)
	}
	if len(cors.AllowHeaders) > 0 {
		setCors(resp, "Access-Control-Allow-Headers", joinCors(cors.AllowHeaders), cors.OriginOverride)
	}
	if len(cors.ExposeHeaders) > 0 {
		setCors(resp, "Access-Control-Expose-Headers", joinCors(cors.ExposeHeaders), cors.OriginOverride)
	}
	if cors.AllowCredentials {
		setCors(resp, "Access-Control-Allow-Credentials", "true", cors.OriginOverride)
	}
	if cors.HasMaxAge {
		setCors(resp, "Access-Control-Max-Age", strconv.FormatInt(cors.MaxAgeSec, 10), cors.OriginOverride)
	}
}

func setCors(resp http.Header, name, value string, override bool) {
	if override || resp.Get(name) == "" {
		resp.Set(name, value)
	}
}

// corsAllowOrigin resolves the Access-Control-Allow-Origin value. "*" wins
// outright; otherwise the request's Origin is echoed if it is in the allow
// list, in which case the response also varies on Origin.
func corsAllowOrigin(allowOrigins []string, requestOrigin string) (value string, vary bool) {
	for _, o := range allowOrigins {
		if o == "*" {
			return "*", false
		}
	}
	for _, o := range allowOrigins {
		if strings.EqualFold(o, requestOrigin) {
			return requestOrigin, true
		}
	}
	return "", false
}

func joinCors(items []string) string {
	return strings.Join(items, ", ")
}

func addVary(resp http.Header, value string) {
	for _, existing := range resp.Values("Vary") {
		for _, part := range strings.Split(existing, ",") {
			if strings.EqualFold(strings.TrimSpace(part), value) {
				return
			}
		}
	}
	resp.Add("Vary", value)
}
