package pipeline

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"crawler/internal/config"
	"crawler/internal/store"
)

func testConfig(siteName string, starts []string) *config.Config {
	return &config.Config{Sites: []config.Site{{
		Name: siteName, StartURLs: starts,
		Concurrency: 3, RatePerSecond: 10000, Burst: 20,
		Timeout: config.Duration(500 * time.Millisecond), Retries: 0,
		BreakAfter5xx: 20, Pause: config.Duration(50 * time.Millisecond),
		MaxPages: 20, MaxBodyRunes: 2000,
		TimeLayouts: []string{time.RFC3339},
		Selectors: config.Selectors{
			Item: ".item", Title: ".title", Link: ".title",
			ProjectNo: ".project", Section: ".section",
			PublishTime: ".publish", Body: ".body",
			Next: ".next a", PageNum: ".page",
		},
	}}}
}

func itemHTML(key string, publish time.Time, body string) string {
	return fmt.Sprintf(`<li class="item"><a class="title" href="/ann/%s">公告 %s</a>`+
		`<span class="project">%s</span><span class="section"></span>`+
		`<span class="publish">%s</span><div class="body">%s</div></li>`,
		key, key, key, publish.Format(time.RFC3339), body)
}

func pageHTML(items ...string) string {
	return `<!doctype html><html><body><ul>` + strings.Join(items, "") + `</ul></body></html>`
}

func openPipelineStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "crawl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestPaginationStopsOnMissingSelfAndDisorder(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/list/1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `%s<div class="next"><a href="/list/2">下一页</a></div>`, pageHTML(itemHTML("P-1", now, "正文一")))
	})
	mux.HandleFunc("/list/2", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `%s<div class="next"><a href="/list/2">末页</a></div>`, pageHTML(itemHTML("P-2", now.Add(time.Second), "正文二")))
	})
	mux.HandleFunc("/disorder/1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `%s<span class="page">1</span><div class="next"><a href="/disorder/3?page=3">下一页</a></div>`, pageHTML(itemHTML("P-3", now, "正文三")))
	})
	mux.HandleFunc("/disorder/3", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `%s<span class="page">3</span><div class="next"><a href="/disorder/2?page=2">乱序</a></div>`, pageHTML(itemHTML("P-4", now, "正文四")))
	})
	mux.HandleFunc("/missing/1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, pageHTML(itemHTML("P-5", now, "正文五")))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	st := openPipelineStore(t)
	cfg := testConfig("s1", []string{srv.URL + "/list/1", srv.URL + "/disorder/1", srv.URL + "/missing/1"})
	stats, err := New(cfg, st).Run(context.Background(), ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Fetched != 5 || stats.RawURL != 5 || stats.New != 5 || stats.Rows != 5 {
		t.Fatalf("unexpected pagination stats: %+v", stats)
	}
}

func TestIncrementalLookbackWatermarkAndIdempotence(t *testing.T) {
	var phase atomic.Int32
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/full", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, pageHTML(
			itemHTML("P-1", now, "项目一正文"),
			itemHTML("P-2", now.Add(30*time.Minute), "项目二正文"),
		))
	})
	mux.HandleFunc("/incr", func(w http.ResponseWriter, r *http.Request) {
		items := []string{
			itemHTML("P-1", now.Add(10*time.Minute), "项目一正文"),
			itemHTML("P-3", now.Add(5*time.Minute), "项目三正文"),
			itemHTML("P-2", now.Add(40*time.Minute), "项目二修订正文"),
			itemHTML("P-4", now.Add(-time.Minute), "项目四正文"),
		}
		if phase.Load() > 0 {
			// Rerun uses the exact same observations.
			items = append(items, itemHTML("P-5", now.Add(20*time.Minute), "项目五正文"))
		}
		fmt.Fprint(w, pageHTML(items...))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	st := openPipelineStore(t)

	fullCfg := testConfig("s1", []string{srv.URL + "/full"})
	p := New(fullCfg, st)
	full, err := p.Run(context.Background(), ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	if full.New != 2 || full.Watermark != "2026-11-12T09:30:00Z|P-2" {
		t.Fatalf("full: %+v", full)
	}

	incrCfg := testConfig("s1", []string{srv.URL + "/incr"})
	incr, err := New(incrCfg, st).Run(context.Background(), ModeIncremental)
	if err != nil {
		t.Fatal(err)
	}
	if incr.Fetched != 1 || incr.RawURL != 4 || incr.New != 1 || incr.Revised != 1 || incr.Dedup != 1 {
		t.Fatalf("increment: %+v", incr)
	}
	if incr.Watermark != "2026-11-12T09:40:00Z|P-2" {
		t.Fatalf("watermark: %s", incr.Watermark)
	}
	if incr.Rows != 3 {
		t.Fatalf("rows after increment=%d", incr.Rows)
	}

	rerun, err := New(incrCfg, st).Run(context.Background(), ModeIncremental)
	if err != nil {
		t.Fatal(err)
	}
	if rerun.New != 0 || rerun.Revised != 0 || rerun.Rows != 3 {
		t.Fatalf("rerun not idempotent: %+v", rerun)
	}

	refull, err := New(fullCfg, st).Run(context.Background(), ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	if refull.New != 0 || refull.Revised != 0 || refull.Rows != 3 {
		t.Fatalf("full rerun not idempotent: %+v", refull)
	}
	if refull.Watermark != "2026-11-12T09:40:00Z|P-2" {
		t.Fatalf("full should recompute watermark from DB: %s", refull.Watermark)
	}
}

func TestFailureClassesAndReplayParse(t *testing.T) {
	var parseMode atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/timeout", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		fmt.Fprint(w, pageHTML(itemHTML("P-1", time.Now().UTC(), "正文")))
	})
	mux.HandleFunc("/404", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	mux.HandleFunc("/500", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	mux.HandleFunc("/429", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) })
	mux.HandleFunc("/parse", func(w http.ResponseWriter, r *http.Request) {
		if parseMode.Load() == 1 {
			fmt.Fprint(w, pageHTML(itemHTML("P-9", time.Now().UTC(), "正文")))
			return
		}
		fmt.Fprint(w, `<html><body><div>broken selector fixture</div></body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	refused := "http://" + closed.Addr().String() + "/refused"
	closed.Close()
	st := openPipelineStore(t)
	cfg := testConfig("s1", []string{
		srv.URL + "/timeout", refused, srv.URL + "/404", srv.URL + "/500", srv.URL + "/429", srv.URL + "/parse",
	})
	cfg.Sites[0].Timeout = config.Duration(30 * time.Millisecond)
	stats, err := New(cfg, st).Run(context.Background(), ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Fetched != 6 || stats.Failed != 6 || stats.New != 0 {
		t.Fatalf("failure run: %+v", stats)
	}
	want := map[string]int{"timeout": 1, "refused": 1, "http_4xx": 1, "http_5xx": 1, "rate_limited": 1, "parse": 1}
	for class, n := range want {
		if stats.Failures[class] != n {
			t.Fatalf("%s=%d want %d (%+v)", class, stats.Failures[class], n, stats.Failures)
		}
	}
	parseMode.Store(1)
	replay, err := New(cfg, st).Replay(context.Background(), "parse")
	if err != nil {
		t.Fatal(err)
	}
	if replay.New != 1 || replay.Failed != 0 {
		t.Fatalf("parse replay: %+v", replay)
	}
	pending, err := st.UnresolvedFailures(context.Background(), "parse")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("parse failure not resolved: %d", len(pending))
	}
}

func TestCanceledRunCommitsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, pageHTML(itemHTML("P-1", time.Now().UTC(), "正文")))
	}))
	defer srv.Close()
	st := openPipelineStore(t)
	cfg := testConfig("s1", []string{srv.URL + "/list"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(cfg, st).Run(ctx, ModeFull); err == nil {
		t.Fatal("expected canceled run error")
	}
	rows, _ := st.CountAnnouncements(context.Background())
	if rows != 0 {
		t.Fatalf("canceled run wrote %d rows", rows)
	}
	if _, err := st.ReadWatermark(context.Background(), "incr"); err != store.ErrNoWatermark {
		t.Fatalf("canceled run advanced watermark: %v", err)
	}
}

func TestDetailTemplateFillsFieldsFromAnnouncementPage(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/list", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body><ul><li class="item"><a class="title" href="/ann/1">公告 P-1</a></li></ul></body></html>`)
	})
	mux.HandleFunc("/ann/1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<html><body><h1 class="title">公告 P-1</h1><span class="project">P-1</span>`+
			`<span class="publish">%s</span><div class="body">详情正文</div></body></html>`, now.Format(time.RFC3339))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	st := openPipelineStore(t)
	cfg := testConfig("s1", []string{srv.URL + "/list"})
	cfg.Sites[0].Detail = true
	stats, err := New(cfg, st).Run(context.Background(), ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Fetched != 2 || stats.RawURL != 1 || stats.New != 1 || stats.Rows != 1 {
		t.Fatalf("detail template stats: %+v", stats)
	}
}
