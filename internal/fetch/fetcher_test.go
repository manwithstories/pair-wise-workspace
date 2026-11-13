package fetch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"crawler/internal/config"
)

func fetchSite() config.Site {
	return config.Site{
		Name: "s", Concurrency: 2, RatePerSecond: 10000, Burst: 20,
		Timeout: config.Duration(2 * time.Second), RetryBackoff: config.Duration(10 * time.Millisecond),
		BreakAfter5xx: 20, Pause: config.Duration(100 * time.Millisecond),
		UserAgent: "test",
	}
}

func TestFetcherHonorsSiteConcurrency(t *testing.T) {
	var active, max atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		for {
			old := max.Load()
			if n <= old || max.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		active.Add(-1)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	site := fetchSite()
	f := New(site)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := f.Fetch(context.Background(), srv.URL)
			if !resp.OK() {
				t.Errorf("fetch failed: %+v", resp)
			}
		}()
	}
	wg.Wait()
	if got := max.Load(); got > 2 || got < 2 {
		t.Fatalf("site concurrency=%d, want 2", got)
	}
}

func TestFetcherTokenBucket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	site := fetchSite()
	site.Concurrency = 1
	site.RatePerSecond = 20
	site.Burst = 1
	f := New(site)
	start := time.Now()
	for i := 0; i < 6; i++ {
		if resp := f.Fetch(context.Background(), srv.URL); !resp.OK() {
			t.Fatalf("fetch %d: %+v", i, resp)
		}
	}
	if elapsed := time.Since(start); elapsed < 180*time.Millisecond {
		t.Fatalf("token bucket did not throttle: %s", elapsed)
	}
}

func TestFetcherPausesAfterConsecutive5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	site := fetchSite()
	site.Concurrency = 1
	site.BreakAfter5xx = 2
	site.Pause = config.Duration(100 * time.Millisecond)
	f := New(site)
	_ = f.Fetch(context.Background(), srv.URL)
	_ = f.Fetch(context.Background(), srv.URL)
	start := time.Now()
	resp := f.Fetch(context.Background(), srv.URL)
	if resp.Class != ClassHTTP5xx {
		t.Fatalf("class=%s", resp.Class)
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("site was not paused after consecutive 5xx: %s", elapsed)
	}
}
