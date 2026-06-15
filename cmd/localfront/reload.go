package main

import (
	"log/slog"
	"sync"
	"time"

	"github.com/mackee/localfront/internal/cffunc"
	"github.com/mackee/localfront/internal/dataplane"
	"github.com/mackee/localfront/internal/origin"
)

// funcCloseGrace is how long a superseded generation of compiled functions is
// kept alive after a reload, so in-flight requests still holding it can drain
// before its QuickJS runtimes are closed.
const funcCloseGrace = 30 * time.Second

// reloader rebuilds the configuration and compiled functions on demand and
// swaps them into the running server atomically. A failed reload leaves the
// current configuration untouched.
type reloader struct {
	opts   *serveOptions
	s3     origin.Fetcher
	server *dataplane.Server
	logger *slog.Logger

	mu           sync.Mutex
	currentFuncs map[string]*cffunc.Function
}

// reload loads the templates, recompiles the functions, and swaps both into the
// server. On any error it returns without touching the running configuration.
func (rl *reloader) reload() error {
	cfg, err := loadConfig(rl.opts, rl.logger)
	if err != nil {
		return err
	}
	funcs, err := buildFunctions(cfg, rl.s3, rl.opts.kvsSeeds, rl.logger)
	if err != nil {
		return err
	}

	rl.mu.Lock()
	old := rl.currentFuncs
	rl.currentFuncs = funcs
	rl.mu.Unlock()

	rl.server.Swap(cfg, funcs)

	if len(old) > 0 {
		time.AfterFunc(funcCloseGrace, func() { closeFunctions(old) })
	}
	return nil
}

// closeCurrent releases the live functions, for shutdown.
func (rl *reloader) closeCurrent() {
	rl.mu.Lock()
	funcs := rl.currentFuncs
	rl.currentFuncs = nil
	rl.mu.Unlock()
	closeFunctions(funcs)
}
