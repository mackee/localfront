// Package dataplane implements the HTTP server that emulates the CloudFront
// data plane: host-based distribution routing, behavior selection, origin
// fetches, and CloudFront request/response headers.
package dataplane

import (
	"crypto/rsa"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mackee/localfront/internal/accesslog"
	"github.com/mackee/localfront/internal/behavior"
	"github.com/mackee/localfront/internal/cffunc"
	"github.com/mackee/localfront/internal/config"
	"github.com/mackee/localfront/internal/origin"
	"github.com/mackee/localfront/internal/sign"
)

// Server serves the data plane for one Config. The active configuration is
// swappable at runtime, which is what template hot reload builds on.
type Server struct {
	logger    *slog.Logger
	snap      atomic.Pointer[snapshot]
	transport http.RoundTripper
	s3        origin.Fetcher
	// now returns the current time, read for access-log timing (request start,
	// first byte, end). Defaults to time.Now (set in New); tests override it for
	// deterministic durations.
	now func() time.Time
	// publicHost is the host (optionally host:port) used as the resource host in
	// signed URL/cookie verification: canned-policy reconstruction and
	// custom-policy Resource host matching. Empty means: use the viewer's Host
	// header as received (see sign.Verify).
	publicHost string
	// accessLog emits one CloudFront Standard log line per completed request.
	// Nil disables access logging; the wrapping in ServeHTTP becomes a no-op.
	accessLog *accesslog.Writer
}

// snapshot is the swappable per-config state: host routing, the compiled
// CloudFront Functions (keyed by logical ID), and the parsed signing public
// keys (keyed by key-pair ID). In-flight requests keep the snapshot they
// started with.
type snapshot struct {
	routes  *routeTable
	funcs   map[string]*cffunc.Function
	pubKeys map[string]*rsa.PublicKey
}

// trustedKeys returns the RSA keys of every public key in a behavior's trusted
// key groups, for signed-URL verification.
func (snap *snapshot) trustedKeys(beh *config.Behavior) []sign.Key {
	var keys []sign.Key
	for _, kg := range beh.TrustedKeyGroups {
		for _, pk := range kg.Keys {
			if rsaKey := snap.pubKeys[pk.ID]; rsaKey != nil {
				keys = append(keys, sign.Key{ID: pk.ID, RSA: rsaKey})
			}
		}
	}
	return keys
}

// Option configures a Server.
type Option func(*Server)

// WithS3Fetcher sets the fetcher used to serve S3 origins. Without one, S3
// origins respond with 502.
func WithS3Fetcher(f origin.Fetcher) Option {
	return func(s *Server) { s.s3 = f }
}

// WithPublicHost sets the host (optionally host:port) used as the resource host
// in signed URL/cookie verification (canned reconstruction and custom-policy
// Resource host matching). Leave it empty to derive the host from each
// request's Host header as received.
func WithPublicHost(host string) Option {
	return func(s *Server) { s.publicHost = host }
}

// WithClock overrides the clock the data plane reads for access-log timing
// (request start, first byte, end). Defaults to time.Now; tests inject a
// deterministic clock so time-taken / time-to-first-byte do not depend on how
// fast the machine completes a request.
func WithClock(now func() time.Time) Option {
	return func(s *Server) { s.now = now }
}

// WithAccessLog directs per-request access logs (CloudFront Standard format)
// at the given writer. Pass nil to disable logging.
func WithAccessLog(w *accesslog.Writer) Option {
	return func(s *Server) { s.accessLog = w }
}

// New returns a Server serving cfg.
func New(cfg *config.Config, logger *slog.Logger, opts ...Option) *Server {
	s := &Server{
		logger:    logger,
		transport: newTransport(),
		now:       time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	s.SwapConfig(cfg)
	return s
}

// SwapConfig atomically replaces the active configuration without functions.
func (s *Server) SwapConfig(cfg *config.Config) {
	s.Swap(cfg, nil)
}

// Swap atomically replaces the active configuration and its compiled functions.
func (s *Server) Swap(cfg *config.Config, funcs map[string]*cffunc.Function) {
	s.snap.Store(&snapshot{routes: buildRoutes(cfg), funcs: funcs, pubKeys: s.parsePublicKeys(cfg)})
}

// parsePublicKeys parses every PublicKey's PEM into an RSA key, keyed by its
// key-pair ID. Keys that fail to parse are skipped with a warning; signed-URL
// verification against them then always denies.
func (s *Server) parsePublicKeys(cfg *config.Config) map[string]*rsa.PublicKey {
	if len(cfg.PublicKeys) == 0 {
		return nil
	}
	out := make(map[string]*rsa.PublicKey, len(cfg.PublicKeys))
	for _, pk := range cfg.PublicKeys {
		key, err := sign.ParsePublicKey(pk.EncodedKey)
		if err != nil {
			s.logger.Warn("could not parse public key; signed-URL verification will deny it",
				"publicKey", pk.LogicalID, "error", err)
			continue
		}
		out[pk.ID] = key
	}
	return out
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
			// Hostnames are case-insensitive and match() looks up a lowercased
			// host, so store aliases lowercased too.
			alias = strings.ToLower(alias)
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
	start := s.now()
	var rec *accessRecorder
	if s.accessLog != nil {
		rec = newAccessRecorder(w, start, r.Proto, s.now)
		rec.wrapRequestBody(r)
		w = rec
	}

	host := hostOnly(r.Host)
	snap := s.snap.Load()
	dist := snap.routes.match(host)
	if dist == nil {
		s.logger.Info("no distribution matches host", "host", host)
		writeCFError(w, http.StatusForbidden, requestID,
			"The request could not be satisfied: no distribution matches the Host header.")
		s.emitAccessLog(r, rec, nil, requestID)
		return
	}

	beh := behavior.Select(dist, r.URL.Path)

	if !allowsMethod(beh.AllowedMethods, r.Method) {
		// Fidelity note: real CloudFront answers a disallowed method with a
		// 403 "request could not be satisfied" page; the exact status and body
		// are still to be verified against a live distribution.
		s.logger.Info("method not allowed by behavior", "method", r.Method, "distribution", dist.LogicalID)
		writeCFError(w, http.StatusForbidden, requestID,
			"This distribution is not configured to allow the HTTP request method that was used for this request.")
		s.emitAccessLog(r, rec, dist, requestID)
		return
	}

	s.serve(w, r, dist, beh, snap, requestID)
	s.emitAccessLog(r, rec, dist, requestID)
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
