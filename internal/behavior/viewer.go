package behavior

import (
	"net/http"
	"strconv"
	"strings"
)

// localfrontViewerPrefix is the request-header prefix used to override the
// synthesized CloudFront-Viewer-* / CloudFront-Is-* values for a single
// request. "X-Localfront-Viewer-Country: JP" sets CloudFront-Viewer-Country.
const localfrontViewerPrefix = "X-Localfront-"

// cloudFrontHeaderPrefix replaces localfrontViewerPrefix on override headers.
const cloudFrontHeaderPrefix = "CloudFront-"

// SynthesizeViewerHeaders builds the CloudFront-Viewer-* / CloudFront-Is-* /
// CloudFront-Forwarded-Proto headers localfront fabricates for a request, then
// applies per-request overrides from X-Localfront-* headers. The result is the
// value pool the origin-request policy draws CloudFront headers from and that
// CloudFront Functions later observe.
func SynthesizeViewerHeaders(r *http.Request) http.Header {
	h := http.Header{}
	h.Set("CloudFront-Forwarded-Proto", "http") // the viewer side is always plain HTTP
	h.Set("CloudFront-Viewer-Http-Version", httpVersion(r.ProtoMajor, r.ProtoMinor))
	// CloudFront-Viewer-Address carries the full "ip:port" of the viewer; the
	// IP-only value is surfaced separately as event.viewer.ip.
	h.Set("CloudFront-Viewer-Address", r.RemoteAddr)
	h.Set("CloudFront-Viewer-Country", "US")
	h.Set("CloudFront-Viewer-TLS", "")
	// Device detection defaults to desktop; override the flags per request to
	// exercise device-dependent code paths.
	h.Set("CloudFront-Is-Desktop-Viewer", "true")
	h.Set("CloudFront-Is-Mobile-Viewer", "false")
	h.Set("CloudFront-Is-Tablet-Viewer", "false")
	h.Set("CloudFront-Is-SmartTV-Viewer", "false")
	h.Set("CloudFront-Is-Android-Viewer", "false")
	h.Set("CloudFront-Is-IOS-Viewer", "false")

	applyViewerOverrides(h, r.Header)
	return h
}

// applyViewerOverrides maps every X-Localfront-<suffix> request header to the
// corresponding CloudFront-<suffix> header, overwriting the synthesized value.
func applyViewerOverrides(dst, src http.Header) {
	for name, values := range src {
		suffix, ok := strings.CutPrefix(name, localfrontViewerPrefix)
		if !ok || suffix == "" {
			continue
		}
		dst[http.CanonicalHeaderKey(cloudFrontHeaderPrefix+suffix)] = append([]string(nil), values...)
	}
}

func httpVersion(major, minor int) string {
	switch major {
	case 2:
		return "HTTP/2.0"
	case 3:
		return "HTTP/3.0"
	case 0:
		return "HTTP/1.1"
	default:
		return "HTTP/1." + strconv.Itoa(minor)
	}
}
