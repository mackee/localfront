package dataplane_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/mackee/localfront/internal/config"
	"github.com/mackee/localfront/internal/dataplane"
)

// Concurrent requests during repeated SwapConfig must always see a consistent
// configuration (every request resolves to a valid origin and returns 200),
// never a torn snapshot or a panic.
func TestSwapConfig_AtomicUnderLoad(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	makeCfg := func() *config.Config {
		o := baseOrigin("o1", host, port)
		dist := baseDistribution("D1", "d1.cloudfront.localhost", []string{"shared.example.test"}, o)
		return &config.Config{Distributions: []*config.Distribution{dist}}
	}

	srv := dataplane.New(makeCfg(), newLogger())

	stop := make(chan struct{})
	var swapper sync.WaitGroup
	swapper.Add(1)
	go func() {
		defer swapper.Done()
		for {
			select {
			case <-stop:
				return
			default:
				srv.SwapConfig(makeCfg())
			}
		}
	}()

	var requesters sync.WaitGroup
	for g := 0; g < 8; g++ {
		requesters.Add(1)
		go func() {
			defer requesters.Done()
			for i := 0; i < 500; i++ {
				req := httptest.NewRequest(http.MethodGet, "http://shared.example.test/", nil)
				req.Host = "shared.example.test"
				rr := httptest.NewRecorder()
				srv.ServeHTTP(rr, req)
				if rr.Code != http.StatusOK {
					t.Errorf("request saw status %d, want 200", rr.Code)
					return
				}
			}
		}()
	}

	requesters.Wait()
	close(stop)
	swapper.Wait()
}
