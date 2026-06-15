package dataplane

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/mackee/localfront/internal/behavior"
	"github.com/mackee/localfront/internal/config"
)

func newTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// Fidelity note: CloudFront's origin connection timeout defaults to
		// 10 seconds; per-origin ConnectionTimeout/ConnectionAttempts are not
		// wired into the dialer yet.
		DialContext:       (&net.Dialer{Timeout: customOriginDialTimeout}).DialContext,
		ForceAttemptHTTP2: true,
		MaxIdleConns:      100,
		IdleConnTimeout:   90 * time.Second,
	}
}

// fetchCustomOrigin proxies the request to a custom HTTP(S) origin, sending
// only the viewer values selected by the behavior's policies plus the headers
// CloudFront sets itself.
func (s *Server) fetchCustomOrigin(ctx context.Context, r *http.Request, dist *config.Distribution, beh *config.Behavior, originReq behavior.OriginRequest, requestID string) *originResponse {
	origin := beh.Origin
	co := origin.Custom

	scheme := "http"
	if co.ProtocolPolicy == "https-only" {
		scheme = "https"
	}
	// match-viewer stays http: localfront's viewer side is always plain HTTP.
	port := co.HTTPPort
	if scheme == "https" {
		port = co.HTTPSPort
	}
	hostport := co.Host
	if (scheme == "http" && port != 80) || (scheme == "https" && port != 443) {
		hostport = net.JoinHostPort(co.Host, strconv.Itoa(port))
	}

	outURL := *r.URL
	outURL.Scheme = scheme
	outURL.Host = hostport
	outURL.Path = origin.OriginPath + r.URL.Path
	if r.URL.RawPath != "" {
		outURL.RawPath = origin.OriginPath + r.URL.RawPath
	}
	outURL.RawQuery = originReq.RawQuery

	reqCtx, cancel := context.WithTimeout(ctx, co.ReadTimeout)
	out, err := http.NewRequestWithContext(reqCtx, r.Method, outURL.String(), r.Body)
	if err != nil {
		cancel()
		s.logger.Error("building origin request failed", "error", err)
		return nil
	}
	out.ContentLength = r.ContentLength

	for name, values := range originReq.Headers {
		out.Header[name] = append([]string(nil), values...)
	}
	addAlwaysForwardedHeaders(out.Header, r.Header)
	removeHopByHopHeaders(out.Header)
	if originReq.AcceptEncoding != "" {
		out.Header.Set("Accept-Encoding", originReq.AcceptEncoding)
	}

	out.Host = co.Host
	if originReq.ForwardHost {
		out.Host = r.Host
	}
	for _, h := range origin.CustomHeaders {
		out.Header.Set(h.Name, h.Value)
	}
	appendXForwardedFor(out.Header, r.Header.Get("X-Forwarded-For"), r.RemoteAddr)
	out.Header.Set("X-Amz-Cf-Id", requestID)
	addVia(out.Header, dist.DomainName)

	resp, err := s.transport.RoundTrip(out)
	if err != nil {
		cancel()
		s.logger.Error("origin fetch failed", "origin", origin.ID, "url", outURL.String(), "error", err)
		return nil
	}
	return &originResponse{
		statusCode: resp.StatusCode,
		header:     resp.Header,
		body:       &cancelOnClose{ReadCloser: resp.Body, cancel: cancel},
	}
}

// addAlwaysForwardedHeaders copies the headers CloudFront forwards to origins
// regardless of policy (Range and conditional headers).
func addAlwaysForwardedHeaders(dst, src http.Header) {
	for _, name := range alwaysForwardedHeaders {
		if v := src.Values(name); len(v) > 0 {
			dst[http.CanonicalHeaderKey(name)] = append([]string(nil), v...)
		}
	}
}

// cancelOnClose cancels the per-request context when the body is closed, so the
// read timeout does not leak past the response lifetime.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}
