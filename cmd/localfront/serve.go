package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mackee/localfront/internal/cfntmpl"
	"github.com/mackee/localfront/internal/config"
	"github.com/mackee/localfront/internal/dataplane"
)

func serve(ctx context.Context, opts *serveOptions, logger *slog.Logger) error {
	sources := make([]cfntmpl.Source, 0, len(opts.templates))
	for _, path := range opts.templates {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sources = append(sources, cfntmpl.Source{Name: path, Data: data})
	}
	cfg, err := config.Load(sources, opts.parameters)
	if err != nil {
		return err
	}
	for _, warning := range cfg.Warnings {
		logger.Warn(warning)
	}

	server := dataplane.New(cfg, logger)
	httpServer := &http.Server{
		Handler:           server,
		ReadHeaderTimeout: 30 * time.Second,
	}
	listener, err := net.Listen("tcp", opts.listen)
	if err != nil {
		return err
	}

	fmt.Printf("data plane   http://%s\n", displayAddr(listener.Addr().String()))
	for _, t := range opts.templates {
		fmt.Printf("template     %s\n", t)
	}
	for _, d := range cfg.Distributions {
		logger.Info("distribution loaded",
			"logicalID", d.LogicalID,
			"id", d.ID,
			"hosts", strings.Join(d.Hostnames(), ", "),
			"enabled", d.Enabled,
		)
	}

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
			return err
		}
		if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
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
