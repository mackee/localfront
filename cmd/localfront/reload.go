package main

import (
	"context"
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
	ctx    context.Context
	opts   *serveOptions
	s3     origin.Fetcher
	server *dataplane.Server
	logger *slog.Logger

	mu           sync.Mutex
	currentFuncs map[string]*cffunc.Function
	pendingTimer *time.Timer
	pendingFuncs map[string]*cffunc.Function
}

// reload loads the templates, recompiles the functions, and swaps both into the
// server. On any error it returns without touching the running configuration.
func (rl *reloader) reload() error {
	cfg, err := loadConfig(rl.opts, rl.logger)
	if err != nil {
		return err
	}
	funcs, err := buildFunctions(rl.ctx, cfg, rl.s3, rl.opts.kvsSeeds, rl.logger)
	if err != nil {
		return err
	}

	rl.mu.Lock()
	// A still-pending grace period for an earlier generation is collapsed now:
	// stopping the timer and closing those functions before scheduling the new
	// grace period keeps a single delayed-close goroutine in flight and avoids
	// any chance of double-closing the same Function on shutdown.
	prevTimer, prevFuncs := rl.pendingTimer, rl.pendingFuncs
	old := rl.currentFuncs
	rl.currentFuncs = funcs
	rl.pendingTimer = nil
	rl.pendingFuncs = nil
	if len(old) > 0 {
		captured := old
		rl.pendingFuncs = captured
		rl.pendingTimer = time.AfterFunc(funcCloseGrace, func() {
			rl.mu.Lock()
			if rl.pendingFuncs == nil {
				rl.mu.Unlock()
				return
			}
			toClose := rl.pendingFuncs
			rl.pendingFuncs = nil
			rl.pendingTimer = nil
			rl.mu.Unlock()
			closeFunctions(toClose)
		})
	}
	rl.mu.Unlock()

	rl.server.Swap(cfg, funcs)

	if prevTimer != nil && prevTimer.Stop() {
		closeFunctions(prevFuncs)
	}
	return nil
}

// closeCurrent releases the live functions, for shutdown.
func (rl *reloader) closeCurrent() {
	rl.mu.Lock()
	timer := rl.pendingTimer
	pending := rl.pendingFuncs
	funcs := rl.currentFuncs
	rl.currentFuncs = nil
	rl.pendingTimer = nil
	rl.pendingFuncs = nil
	rl.mu.Unlock()

	if timer != nil && timer.Stop() {
		closeFunctions(pending)
	}
	closeFunctions(funcs)
}
