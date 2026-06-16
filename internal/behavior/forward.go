package behavior

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/mackee/localfront/internal/config"
)

// OriginRequest is the viewer-derived part of a request that a behavior
// forwards to its origin, after cache policy and origin request policy are
// applied. The data plane adds the headers it sets itself (X-Forwarded-For,
// Via, X-Amz-Cf-Id, origin custom headers) on top of this.
type OriginRequest struct {
	// Headers are the viewer and synthesized CloudFront headers that reach the
	// origin. The Host header is reported via ForwardHost instead, and the
	// Cookie header is rebuilt from the forwarded cookies.
	Headers http.Header
	// RawQuery is the filtered query string, in the viewer's original order.
	RawQuery string
	// AcceptEncoding is the value to send to the origin ("" means: send none).
	AcceptEncoding string
	// ForwardHost reports whether the viewer's Host header should be forwarded
	// instead of being replaced with the origin's domain name.
	ForwardHost bool
}

// BuildOriginRequest applies the behavior's cache and origin request policies
// to a viewer request and returns what reaches the origin. viewerHeaders is the
// synthesized CloudFront-* pool from SynthesizeViewerHeaders; values are pulled
// from it whenever a policy selects a CloudFront-* header.
func BuildOriginRequest(b *config.Behavior, r *http.Request, viewerHeaders http.Header) OriginRequest {
	cache := b.CachePolicy
	orp := b.OriginRequestPolicy

	out := OriginRequest{Headers: http.Header{}}

	// Header names the viewer flagged as connection-specific (RFC 9110): these
	// are hop-by-hop and must never reach the origin, even if a policy
	// whitelists them. They are collected here, while the viewer's Connection
	// header is still in scope — the data plane strips the standard hop-by-hop
	// set later, but by then the filtered header set no longer carries the
	// Connection header that names them.
	connectionListed := connectionListedHeaders(r.Header)

	// 1. Viewer-supplied headers, filtered by the union of both policies. Host,
	// Cookie, and Accept-Encoding are handled separately; control headers,
	// connection-specific headers, and any viewer-supplied CloudFront-* headers
	// are dropped (the remaining hop-by-hop headers are stripped by the data
	// plane).
	for name, values := range r.Header {
		switch {
		case strings.EqualFold(name, "Host"),
			strings.EqualFold(name, "Cookie"),
			strings.EqualFold(name, "Accept-Encoding"),
			isCloudFrontHeader(name),
			connectionListed[http.CanonicalHeaderKey(name)],
			hasPrefixFold(name, localfrontViewerPrefix):
			continue
		}
		if forwardsHeader(cache, orp, name) {
			out.Headers[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
	}

	// 2. Synthesized CloudFront-* headers selected by a whitelist or by the
	// origin request policy's allViewerAndWhitelistCloudFront list.
	for name, values := range viewerHeaders {
		if selectsCloudFrontHeader(cache, orp, name) {
			out.Headers[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
	}

	out.ForwardHost = forwardsHeader(cache, orp, "Host")
	if cookie := filterCookies(cache, orp, r.Header.Get("Cookie")); cookie != "" {
		out.Headers.Set("Cookie", cookie)
	}
	out.RawQuery = filterQuery(cache, orp, r.URL.RawQuery)
	out.AcceptEncoding = normalizeAcceptEncoding(cache, orp, r.Header.Get("Accept-Encoding"))
	return out
}

func isCloudFrontHeader(name string) bool {
	return hasPrefixFold(name, cloudFrontHeaderPrefix)
}

// connectionListedHeaders returns the canonicalized set of header names the
// viewer named in its Connection header (e.g. "Connection: keep-alive, X-Hop"
// flags X-Hop). Per RFC 9110 these are connection-specific and must be removed
// before the request is forwarded.
func connectionListedHeaders(h http.Header) map[string]bool {
	var listed map[string]bool
	for _, value := range h.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				if listed == nil {
					listed = map[string]bool{}
				}
				listed[http.CanonicalHeaderKey(token)] = true
			}
		}
	}
	return listed
}

// hasPrefixFold reports whether s starts with prefix, case-insensitively. It is
// used on header names, which Go canonicalizes to "Cloudfront-"/"X-Localfront-"
// rather than the mixed case of our prefix constants.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// forwardsHeader reports whether a viewer header (or Host) reaches the origin,
// taking the union of the cache policy and origin request policy selections.
func forwardsHeader(cache *config.CachePolicy, orp *config.OriginRequestPolicy, name string) bool {
	if cache != nil && headerSelected(cache.Headers, name, false) {
		return true
	}
	if orp != nil && headerSelected(orp.Headers, name, true) {
		return true
	}
	return false
}

// headerSelected applies one ListSelection to a viewer header name. allowViewer
// distinguishes origin request policies (which support allViewer / allExcept)
// from cache policies (which support only none / whitelist for headers).
func headerSelected(sel config.ListSelection, name string, allowViewer bool) bool {
	switch sel.Behavior {
	case "all":
		// Legacy ForwardedValues with Headers: ['*'] forwards every viewer
		// header (mapped to "all" by cachePolicyFromForwardedValues).
		return true
	case "whitelist":
		return sel.ContainsFold(name)
	case "allViewer", "allViewerAndWhitelistCloudFront":
		return allowViewer
	case "allExcept":
		return allowViewer && !sel.ContainsFold(name)
	default: // none / unset
		return false
	}
}

// selectsCloudFrontHeader reports whether a synthesized CloudFront-* header is
// forwarded. Only an explicit whitelist (cache or ORP) or the ORP's
// allViewerAndWhitelistCloudFront list adds these; plain allViewer does not.
func selectsCloudFrontHeader(cache *config.CachePolicy, orp *config.OriginRequestPolicy, name string) bool {
	if cache != nil && cache.Headers.Behavior == "whitelist" && cache.Headers.ContainsFold(name) {
		return true
	}
	if orp == nil {
		return false
	}
	switch orp.Headers.Behavior {
	case "whitelist", "allViewerAndWhitelistCloudFront":
		return orp.Headers.ContainsFold(name)
	default:
		return false
	}
}

// filterCookies rebuilds the Cookie header, keeping cookies selected by either
// policy and preserving their original order.
func filterCookies(cache *config.CachePolicy, orp *config.OriginRequestPolicy, cookieHeader string) string {
	if cookieHeader == "" {
		return ""
	}
	var kept []string
	for _, pair := range strings.Split(cookieHeader, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name := pair
		if eq := strings.IndexByte(pair, '='); eq >= 0 {
			name = pair[:eq]
		}
		if cookieSelected(cache, orp, name) {
			kept = append(kept, pair)
		}
	}
	return strings.Join(kept, "; ")
}

func cookieSelected(cache *config.CachePolicy, orp *config.OriginRequestPolicy, name string) bool {
	if cache != nil && listSelected(cache.Cookies, name) {
		return true
	}
	if orp != nil && listSelected(orp.Cookies, name) {
		return true
	}
	return false
}

// filterQuery keeps query-string parameters selected by either policy, in the
// viewer's original order and with the original encoding.
func filterQuery(cache *config.CachePolicy, orp *config.OriginRequestPolicy, rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	var kept []string
	for _, param := range strings.Split(rawQuery, "&") {
		if param == "" {
			continue
		}
		key := param
		if eq := strings.IndexByte(param, '='); eq >= 0 {
			key = param[:eq]
		}
		// Query-string names are matched after URL-decoding the key.
		decoded := key
		if d, err := url.QueryUnescape(key); err == nil {
			decoded = d
		}
		if querySelected(cache, orp, decoded) {
			kept = append(kept, param)
		}
	}
	return strings.Join(kept, "&")
}

func querySelected(cache *config.CachePolicy, orp *config.OriginRequestPolicy, name string) bool {
	if cache != nil && listSelected(cache.QueryStrings, name) {
		return true
	}
	if orp != nil && listSelected(orp.QueryStrings, name) {
		return true
	}
	return false
}

// listSelected applies the none / whitelist / all / allExcept behaviors shared
// by cookies and query strings.
func listSelected(sel config.ListSelection, name string) bool {
	switch sel.Behavior {
	case "all":
		return true
	case "whitelist":
		return sel.Contains(name)
	case "allExcept":
		return !sel.Contains(name)
	default: // none / unset
		return false
	}
}

// normalizeAcceptEncoding reproduces CloudFront's Accept-Encoding handling:
// when the cache policy enables gzip and/or Brotli, the header sent to the
// origin is normalized to the enabled encodings the viewer also accepts
// (gzip listed first). When neither is enabled, Accept-Encoding only reaches
// the origin if a policy explicitly forwards it.
func normalizeAcceptEncoding(cache *config.CachePolicy, orp *config.OriginRequestPolicy, viewer string) string {
	gzipEnabled := cache != nil && cache.Gzip
	brotliEnabled := cache != nil && cache.Brotli
	if !gzipEnabled && !brotliEnabled {
		if forwardsHeader(cache, orp, "Accept-Encoding") {
			return viewer
		}
		return ""
	}
	accepts := parseAcceptEncoding(viewer)
	var enc []string
	if gzipEnabled && accepts["gzip"] {
		enc = append(enc, "gzip")
	}
	if brotliEnabled && accepts["br"] {
		enc = append(enc, "br")
	}
	return strings.Join(enc, ", ")
}

// parseAcceptEncoding returns the set of encodings the viewer accepts, ignoring
// q-values (an encoding with q=0 is treated as accepted for the PoC).
func parseAcceptEncoding(v string) map[string]bool {
	out := map[string]bool{}
	for _, token := range strings.Split(v, ",") {
		token = strings.TrimSpace(token)
		if i := strings.IndexByte(token, ';'); i >= 0 {
			token = strings.TrimSpace(token[:i])
		}
		if token != "" {
			out[strings.ToLower(token)] = true
		}
	}
	return out
}
