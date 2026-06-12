package dataplane

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/mackee/localfront/internal/config"
)

func newTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// Fidelity note: CloudFront's origin connection timeout defaults to
		// 10 seconds; per-origin ConnectionTimeout/ConnectionAttempts are not
		// wired into the dialer yet (TODO M4).
		DialContext:       (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		ForceAttemptHTTP2: true,
		MaxIdleConns:      100,
		IdleConnTimeout:   90 * time.Second,
	}
}

func (s *Server) proxyCustomOrigin(w http.ResponseWriter, r *http.Request, dist *config.Distribution, behavior *config.Behavior, requestID string) {
	origin := behavior.Origin
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
	// TODO(M4): cache policy / origin request policy decide which headers,
	// cookies, and query strings reach the origin; M2 forwards everything.

	ctx, cancel := context.WithTimeout(r.Context(), co.ReadTimeout)
	defer cancel()
	out, err := http.NewRequestWithContext(ctx, r.Method, outURL.String(), r.Body)
	if err != nil {
		s.logger.Error("building origin request failed", "error", err)
		writeCFError(w, http.StatusInternalServerError, requestID, "localfront failed to build the origin request.")
		return
	}
	out.ContentLength = r.ContentLength

	copyHeaders(out.Header, r.Header)
	removeHopByHopHeaders(out.Header)
	out.Host = co.Host
	for _, h := range origin.CustomHeaders {
		out.Header.Set(h.Name, h.Value)
	}
	appendXForwardedFor(out.Header, r.RemoteAddr)
	out.Header.Set("X-Amz-Cf-Id", requestID)
	addVia(out.Header, dist.DomainName)

	resp, err := s.transport.RoundTrip(out)
	if err != nil {
		s.logger.Error("origin fetch failed", "origin", origin.ID, "url", outURL.String(), "error", err)
		writeCFError(w, http.StatusBadGateway, requestID,
			"localfront wasn't able to connect to the origin.")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	copyHeaders(w.Header(), resp.Header)
	removeHopByHopHeaders(w.Header())
	addVia(w.Header(), dist.DomainName)
	w.Header().Set("X-Cache", "Miss from localfront")
	w.Header().Set("X-Amz-Cf-Id", requestID)
	w.Header().Set("X-Amz-Cf-Pop", popName)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		s.logger.Debug("copying origin response failed", "error", err)
	}
}
