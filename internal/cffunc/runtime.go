package cffunc

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/fastschema/qjs"
)

// defaultPoolSize is the number of QuickJS runtimes kept warm per function.
// QuickJS runtimes are not goroutine-safe, so each concurrent invocation needs
// its own; the pool grows on demand and caps reuse at this size.
const defaultPoolSize = 8

// Options configures a compiled function.
type Options struct {
	Name     string
	Code     string
	Runtime  string       // cloudfront-js-1.0 / cloudfront-js-2.0 (informational)
	KVS      *KVS         // associated key-value store, or nil
	PoolSize int          // warm runtimes; defaults to defaultPoolSize
	CacheDir string       // wazero compile cache directory; "" disables disk cache
	Log      func(string) // console.log sink; nil discards
}

// Function is a compiled CloudFront Function backed by a pool of QuickJS
// runtimes. It is safe for concurrent use.
type Function struct {
	name    string
	pool    *pool
	sandbox string // empty temp dir mounted as the wasm CWD
}

var importLine = regexp.MustCompile(`(?m)^\s*import\s`)

// Compile builds a Function, validating the code by instantiating one runtime
// eagerly so syntax errors surface at load time.
func Compile(opts Options) (*Function, error) {
	if opts.Code == "" {
		return nil, fmt.Errorf("function %s has no code", opts.Name)
	}
	size := opts.PoolSize
	if size <= 0 {
		size = defaultPoolSize
	}
	// Confine the WASI filesystem to an empty directory: by default wazero
	// mounts the process CWD at "/", which would expose real files to JS.
	sandbox, err := os.MkdirTemp("", "localfront-cffunc-")
	if err != nil {
		return nil, fmt.Errorf("creating function sandbox dir: %w", err)
	}

	qopt := qjs.Option{CWD: sandbox, CacheDir: opts.CacheDir, Stdout: io.Discard, Stderr: io.Discard}
	p := newPool(size, qopt, opts.setupRuntime)

	f := &Function{name: opts.Name, pool: p, sandbox: sandbox}

	// Force one runtime through setup to validate the code now.
	rt, err := p.get()
	if err != nil {
		_ = os.RemoveAll(sandbox)
		return nil, fmt.Errorf("loading function %s: %w", opts.Name, err)
	}
	p.put(rt)
	return f, nil
}

// Close releases all pooled runtimes and the sandbox directory.
func (f *Function) Close() {
	if f.pool != nil {
		f.pool.close()
	}
	if f.sandbox != "" {
		_ = os.RemoveAll(f.sandbox)
	}
}

// Result is what a handler returned: either a (possibly modified) request to
// continue with, or a response to return directly.
type Result struct {
	Request  *Request
	Response *Response
}

// IsResponse reports whether the handler produced a response (a short-circuit
// for viewer-request, or the modified response for viewer-response).
func (r *Result) IsResponse() bool { return r.Response != nil }

// Execute runs the function against event and returns the handler's result.
// JavaScript exceptions and engine errors are returned as errors; the caller
// maps them to a CloudFront-compatible 503.
func (f *Function) Execute(event *Event) (*Result, error) {
	payload, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	rt, err := f.pool.get()
	if err != nil {
		return nil, err
	}
	reusable := false
	defer func() {
		if reusable {
			f.pool.put(rt)
		} else {
			rt.Close()
		}
	}()

	ctx := rt.Context()
	runFn := ctx.Global().GetPropertyStr("__run")
	arg := ctx.NewString(string(payload))
	res, err := ctx.Invoke(runFn, ctx.NewUndefined(), arg)
	if err != nil {
		runFn.Free()
		arg.Free()
		return nil, fmt.Errorf("function %s: %w", f.name, err)
	}
	resolved, err := res.Await()
	if err != nil {
		runFn.Free()
		arg.Free()
		return nil, fmt.Errorf("function %s: %w", f.name, err)
	}
	out := resolved.String()
	resolved.Free()
	arg.Free()
	runFn.Free()
	reusable = true

	return parseResult(event.Context.EventType, out)
}

// parseResult interprets the handler's JSON output. For viewer-response the
// result is always a response; for viewer-request it is a response when it
// carries a statusCode (a short-circuit), otherwise a modified request.
func parseResult(eventType, out string) (*Result, error) {
	if eventType == "viewer-response" {
		var resp Response
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			return nil, fmt.Errorf("decoding function response: %w", err)
		}
		return &Result{Response: &resp}, nil
	}

	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &probe); err != nil {
		return nil, fmt.Errorf("decoding function result: %w", err)
	}
	if _, isResponse := probe["statusCode"]; isResponse {
		var resp Response
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			return nil, fmt.Errorf("decoding function response: %w", err)
		}
		return &Result{Response: &resp}, nil
	}
	var req Request
	if err := json.Unmarshal([]byte(out), &req); err != nil {
		return nil, fmt.Errorf("decoding function request: %w", err)
	}
	return &Result{Request: &req}, nil
}

// setupRuntime loads the sandbox prelude, the cloudfront module, the user code,
// and the JSON-in/JSON-out trampoline into a fresh runtime.
func (opts Options) setupRuntime(rt *qjs.Runtime) error {
	ctx := rt.Context()
	kvs := opts.KVS

	ctx.SetFunc("__kvsGet", func(this *qjs.This) (*qjs.Value, error) {
		if kvs == nil {
			return nil, fmt.Errorf("no KeyValueStore is associated with this function")
		}
		args := this.Args()
		if len(args) < 1 {
			return nil, fmt.Errorf("kvs get requires a key")
		}
		key := args[0].String()
		v, ok := kvs.Get(key)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrKeyNotFound, key)
		}
		return this.Context().NewString(v), nil
	})
	ctx.SetFunc("__kvsExists", func(this *qjs.This) (*qjs.Value, error) {
		if kvs == nil {
			return this.Context().NewBool(false), nil
		}
		args := this.Args()
		if len(args) < 1 {
			return nil, fmt.Errorf("kvs exists requires a key")
		}
		_, ok := kvs.Get(args[0].String())
		return this.Context().NewBool(ok), nil
	})
	logFn := opts.Log
	ctx.SetFunc("__log", func(this *qjs.This) (*qjs.Value, error) {
		if logFn != nil && len(this.Args()) > 0 {
			logFn(this.Args()[0].String())
		}
		return this.Context().NewUndefined(), nil
	})

	if _, err := ctx.Eval("prelude.js", qjs.Code(preludeJS)); err != nil {
		return fmt.Errorf("prelude: %w", err)
	}
	if _, err := ctx.Load("cloudfront", qjs.Code(cloudfrontModuleJS)); err != nil {
		return fmt.Errorf("cloudfront module: %w", err)
	}

	if importLine.MatchString(opts.Code) {
		code := opts.Code
		if !strings.Contains(code, "export default") {
			code += "\nexport default handler;\n"
		}
		h, err := ctx.Eval("function.js", qjs.Code(code), qjs.TypeModule())
		if err != nil {
			return fmt.Errorf("function code: %w", err)
		}
		if !h.IsFunction() {
			return fmt.Errorf("the function's default export is not a function")
		}
		ctx.Global().SetPropertyStr("__handler", h)
	} else {
		if _, err := ctx.Eval("function.js", qjs.Code(opts.Code)); err != nil {
			return fmt.Errorf("function code: %w", err)
		}
		h := ctx.Global().GetPropertyStr("handler")
		if !h.IsFunction() {
			return fmt.Errorf("the function does not define a global handler()")
		}
		ctx.Global().SetPropertyStr("__handler", h)
	}

	if _, err := ctx.Eval("trampoline.js", qjs.Code(trampolineJS)); err != nil {
		return fmt.Errorf("trampoline: %w", err)
	}
	return nil
}

const preludeJS = `(function (g) {
	g.std = undefined;
	g.os = undefined;
	g.setTimeout = undefined;
	g.setInterval = undefined;
	g.clearTimeout = undefined;
	g.clearInterval = undefined;
	g.print = undefined;
	var emit = function () {
		g.__log(Array.prototype.map.call(arguments, String).join(' '));
	};
	g.console = { log: emit, error: emit, warn: emit, info: emit, debug: emit };
	g.__makeKvsHandle = function () {
		return {
			get: function (key, options) {
				return Promise.resolve().then(function () {
					var v = g.__kvsGet(String(key));
					if (options && options.format === 'json') { return JSON.parse(v); }
					return v;
				});
			},
			exists: function (key) {
				return Promise.resolve().then(function () { return g.__kvsExists(String(key)); });
			},
		};
	};
	g.cf = { kvs: function (id) { return g.__makeKvsHandle(); } };
})(globalThis);
`

const cloudfrontModuleJS = `export default globalThis.cf;`

const trampolineJS = `globalThis.__run = function (eventJson) {
	return Promise.resolve(globalThis.__handler(JSON.parse(eventJson))).then(function (r) {
		return JSON.stringify(r);
	});
};
`
