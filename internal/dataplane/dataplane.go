// Package dataplane implements the HTTP server that emulates the CloudFront
// data plane: host-based distribution routing, behavior selection, origin
// fetches, and CloudFront request/response headers.
package dataplane

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/mackee/localfront/internal/config"
)

// Server serves the data plane for one Config. The active configuration is
// swappable at runtime, which is what template hot reload builds on.
type Server struct {
	logger    *slog.Logger
	routes    atomic.Pointer[routeTable]
	transport http.RoundTripper
}

// New returns a Server serving cfg.
func New(cfg *config.Config, logger *slog.Logger) *Server {
	s := &Server{
		logger:    logger,
		transport: newTransport(),
	}
	s.SwapConfig(cfg)
	return s
}

// SwapConfig atomically replaces the active configuration. In-flight
// requests keep the snapshot they started with.
func (s *Server) SwapConfig(cfg *config.Config) {
	s.routes.Store(buildRoutes(cfg))
}

type routeTable struct {
	exact    map[string]*config.Distribution
	wildcard []wildcardRoute
}

type wildcardRoute struct {
	suffix string // ".example.com" for the alias "*.example.com"
	dist   *config.Distribution
}

func buildRoutes(cfg *config.Config) *routeTable {
	t := &routeTable{exact: map[string]*config.Distribution{}}
	for _, d := range cfg.Distributions {
		if !d.Enabled {
			continue
		}
		t.exact[strings.ToLower(d.DomainName)] = d
		for _, alias := range d.Aliases {
			if rest, ok := strings.CutPrefix(alias, "*"); ok {
				t.wildcard = append(t.wildcard, wildcardRoute{suffix: rest, dist: d})
				continue
			}
			t.exact[alias] = d
		}
	}
	return t
}

func (t *routeTable) match(host string) *config.Distribution {
	if d, ok := t.exact[host]; ok {
		return d
	}
	for _, w := range t.wildcard {
		// A wildcard covers exactly one extra label, like CloudFront CNAMEs.
		label, ok := strings.CutSuffix(host, w.suffix)
		if ok && label != "" && !strings.Contains(label, ".") {
			return w.dist
		}
	}
	return nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := newRequestID()
	host := hostOnly(r.Host)
	dist := s.routes.Load().match(host)
	if dist == nil {
		s.logger.Info("no distribution matches host", "host", host)
		writeCFError(w, http.StatusForbidden, requestID,
			"The request could not be satisfied: no distribution matches the Host header.")
		return
	}

	// TODO(M4): select across CacheBehaviors by path pattern; until then the
	// default behavior serves everything.
	behavior := dist.DefaultBehavior

	if !allowsMethod(behavior.AllowedMethods, r.Method) {
		// Fidelity note: real CloudFront answers a disallowed method with a
		// 403 "request could not be satisfied" page; verify the exact status
		// and body against a live distribution when behaviors land in M4.
		s.logger.Info("method not allowed by behavior", "method", r.Method, "distribution", dist.LogicalID)
		writeCFError(w, http.StatusForbidden, requestID,
			"This distribution is not configured to allow the HTTP request method that was used for this request.")
		return
	}

	switch {
	case behavior.Origin.Custom != nil:
		s.proxyCustomOrigin(w, r, dist, behavior, requestID)
	case behavior.Origin.S3 != nil:
		// S3 origins are implemented in milestone M3.
		s.logger.Warn("S3 origins are not implemented yet", "origin", behavior.Origin.ID)
		writeCFError(w, http.StatusBadGateway, requestID,
			"localfront cannot serve this origin yet: S3 origins are not implemented.")
	default:
		writeCFError(w, http.StatusInternalServerError, requestID, "The origin has no usable configuration.")
	}
}

func hostOnly(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(host)
	}
	return strings.ToLower(hostport)
}

func allowsMethod(methods []string, method string) bool {
	for _, m := range methods {
		if m == method {
			return true
		}
	}
	return false
}
