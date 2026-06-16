package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/mackee/localfront/internal/accesslog"
	"github.com/mackee/localfront/internal/cfntmpl"
	"github.com/mackee/localfront/internal/config"
	"github.com/mackee/localfront/internal/dataplane"
	"github.com/mackee/localfront/internal/origin"
	"github.com/mackee/localfront/internal/watch"
)

func serve(ctx context.Context, opts *serveOptions, logger *slog.Logger) error {
	cfg, err := loadConfig(opts, logger)
	if err != nil {
		return err
	}

	dpOpts := []dataplane.Option{dataplane.WithPublicHost(opts.publicHost)}
	logger.Info("verifying canned signed URLs against the public host", "host", opts.publicHost)

	accessLogCloser, accessLogWriter, err := openAccessLog(opts.accessLog)
	if err != nil {
		return err
	}
	if accessLogCloser != nil {
		defer func() {
			if cerr := accessLogCloser.Close(); cerr != nil {
				logger.Warn("closing access log", "error", cerr)
			}
		}()
	}
	if accessLogWriter != nil {
		dpOpts = append(dpOpts, dataplane.WithAccessLog(accessLogWriter))
		logger.Info("access log enabled", "destination", opts.accessLog)
	}
	var s3Client origin.Fetcher
	if opts.s3Endpoint != "" {
		client, err := origin.NewS3Client(opts.s3Endpoint, opts.s3Region, opts.s3Access, opts.s3Secret, nil)
		if err != nil {
			return fmt.Errorf("configuring object store client for %s: %w", opts.s3Endpoint, err)
		}
		s3Client = client
		dpOpts = append(dpOpts, dataplane.WithS3Fetcher(client))
		logger.Info("object store enabled", "endpoint", opts.s3Endpoint, "region", opts.s3Region)
	}
	if (configUsesS3(cfg) || configUsesKVSImportSource(cfg, opts.kvsSeeds)) && s3Client == nil {
		return fmt.Errorf("the templates use S3 origins or a KeyValueStore ImportSource; provide --s3-endpoint (and --s3-access-key / --s3-secret-key) for the object store")
	}

	funcs, err := buildFunctions(ctx, cfg, s3Client, opts.kvsSeeds, logger)
	if err != nil {
		return err
	}
	if len(funcs) > 0 {
		logger.Info("CloudFront Functions compiled", "count", len(funcs))
	}

	server := dataplane.New(cfg, logger, dpOpts...)
	server.Swap(cfg, funcs)

	rl := &reloader{ctx: ctx, opts: opts, s3: s3Client, server: server, logger: logger, currentFuncs: funcs}
	defer rl.closeCurrent()

	httpServer := &http.Server{
		Handler:           server,
		ReadHeaderTimeout: 30 * time.Second,
	}
	listener, err := net.Listen("tcp", opts.listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", opts.listen, err)
	}

	go watchFiles(ctx, opts, rl, logger)
	printSummary(listener, opts, cfg)

	errc := make(chan error, 1)
	go func() {
		errc <- httpServer.Serve(listener)
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutting down http server: %w", err)
		}
		if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	}
}

// loadConfig reads the template files and builds the resolved configuration,
// logging any warnings.
func loadConfig(opts *serveOptions, logger *slog.Logger) (*config.Config, error) {
	sources := make([]cfntmpl.Source, 0, len(opts.templates))
	for _, path := range opts.templates {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading template %s: %w", path, err)
		}
		sources = append(sources, cfntmpl.Source{Name: path, Data: data})
	}
	cfg, err := config.Load(sources, opts.parameters)
	if err != nil {
		return nil, fmt.Errorf("loading templates: %w", err)
	}
	for _, warning := range cfg.Warnings {
		logger.Warn(warning)
	}
	return cfg, nil
}

// watchFiles watches the templates and seed files and reloads on change,
// keeping the previous configuration on any error.
func watchFiles(ctx context.Context, opts *serveOptions, rl *reloader, logger *slog.Logger) {
	files := append([]string{}, opts.templates...)
	for _, path := range opts.kvsSeeds {
		files = append(files, path)
	}
	onChange := func() {
		if err := rl.reload(); err != nil {
			logger.Error("reload failed; keeping the previous configuration", "error", err)
			return
		}
		logger.Info("configuration reloaded")
	}
	if err := watch.Watch(ctx, files, logger, onChange); err != nil {
		logger.Warn("file watching disabled", "error", err)
	}
}

// printSummary writes the startup banner to stderr. The access log occupies
// stdout when enabled (the default), so keeping the banner off stdout means
// downstream consumers can pipe access logs straight into ETL without first
// stripping the banner lines.
func printSummary(listener net.Listener, opts *serveOptions, cfg *config.Config) {
	fmt.Fprintf(os.Stderr, "data plane   http://%s\n", displayAddr(listener.Addr().String()))
	for _, t := range opts.templates {
		fmt.Fprintf(os.Stderr, "template     %s (hot reload)\n", t)
	}
	for _, d := range cfg.Distributions {
		status := ""
		if !d.Enabled {
			status = " (disabled)"
		}
		fmt.Fprintf(os.Stderr, "distribution %s [%s]%s\n", d.ID, d.LogicalID, status)
		for _, host := range d.Hostnames() {
			fmt.Fprintf(os.Stderr, "  http://%s -> %s\n", host, displayAddr(listener.Addr().String()))
		}
	}
}

// configUsesS3 reports whether any enabled distribution has an S3 origin.
// Disabled distributions are never routed (see buildRoutes), so they must not
// force the --s3-endpoint requirement.
func configUsesS3(cfg *config.Config) bool {
	for _, d := range cfg.Distributions {
		if !d.Enabled {
			continue
		}
		for _, o := range d.Origins {
			if o.S3 != nil {
				return true
			}
		}
	}
	return false
}

// configUsesKVSImportSource reports whether a KeyValueStore reachable from an
// enabled distribution loads its seed data from the object store. Stores reached
// only from disabled distributions are not seeded (see buildKVSStores), so they
// must not force the --s3-endpoint requirement. A store covered by a local
// --kvs-seed is likewise excluded: the seed replaces its ImportSource, so the
// object store is not needed for it (see buildKVSStores).
func configUsesKVSImportSource(cfg *config.Config, seeds map[string]string) bool {
	_, reachableKVS := reachableResources(cfg)
	for _, kvs := range cfg.KeyValueStores {
		if kvs.ImportSourceARN == "" || !reachableKVS[kvs.LogicalID] {
			continue
		}
		if _, _, hasSeed := lookupSeed(seeds, kvs.Name, kvs.LogicalID); hasSeed {
			continue
		}
		return true
	}
	return false
}

// openAccessLog resolves the --access-log destination into a writer suitable
// for accesslog.NewWriter. An empty path returns (nil, nil, nil), meaning
// access logging stays disabled. "-" or "stdout" send the log to os.Stdout,
// keeping it separate from the slog operational stream on stderr. Any other
// path is opened append-only, creating the file if necessary; the caller is
// responsible for closing the io.Closer when the server stops.
func openAccessLog(path string) (io.Closer, *accesslog.Writer, error) {
	if path == "" {
		return nil, nil, nil
	}
	if path == "-" || path == "stdout" {
		w, err := accesslog.NewWriter(os.Stdout)
		if err != nil {
			return nil, nil, fmt.Errorf("initializing access log on stdout: %w", err)
		}
		return nil, w, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("opening access log %s: %w", path, err)
	}
	w, err := accesslog.NewWriter(f)
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("initializing access log %s: %w", path, err)
	}
	return f, w, nil
}

// displayAddr turns a listener address into something clickable.
func displayAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "::" || host == "0.0.0.0" {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}
