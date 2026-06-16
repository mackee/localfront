package cffunc

import (
	"sync"

	"github.com/fastschema/qjs"
)

// pool is a closable pool of fully-initialized QuickJS runtimes. QuickJS
// runtimes are not goroutine-safe, so each concurrent Execute borrows one
// exclusively. Unlike qjs.Pool it can be drained on Close, which template hot
// reload relies on to avoid leaking wasm instances.
type pool struct {
	mu     sync.Mutex
	idle   []*qjs.Runtime
	max    int
	closed bool
	option qjs.Option
	setup  func(*qjs.Runtime) error
}

func newPool(max int, option qjs.Option, setup func(*qjs.Runtime) error) *pool {
	return &pool{max: max, option: option, setup: setup}
}

// get borrows a runtime, creating and initializing a fresh one if none are
// idle. Reused runtimes have their stack top re-anchored for the calling
// goroutine, as QuickJS uses it for stack-overflow detection.
func (p *pool) get() (*qjs.Runtime, error) {
	p.mu.Lock()
	if n := len(p.idle); n > 0 {
		rt := p.idle[n-1]
		p.idle = p.idle[:n-1]
		p.mu.Unlock()
		rt.Call("QJS_UpdateStackTop", rt.Raw())
		return rt, nil
	}
	p.mu.Unlock()

	rt, err := qjs.New(p.option)
	if err != nil {
		return nil, err
	}
	if err := p.setup(rt); err != nil {
		rt.Close()
		return nil, err
	}
	return rt, nil
}

// put returns a runtime for reuse, or closes it when the pool is full or closed.
func (p *pool) put(rt *qjs.Runtime) {
	p.mu.Lock()
	if p.closed || len(p.idle) >= p.max {
		p.mu.Unlock()
		rt.Close()
		return
	}
	p.idle = append(p.idle, rt)
	p.mu.Unlock()
}

// close drains and closes every idle runtime. Runtimes currently borrowed are
// closed by put once returned.
func (p *pool) close() {
	p.mu.Lock()
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()
	for _, rt := range idle {
		rt.Close()
	}
}
