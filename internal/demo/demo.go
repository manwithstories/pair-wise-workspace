package demo

import (
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"

	"crawler/internal/config"
	"crawler/internal/pipeline"
	"crawler/internal/report"
	"crawler/internal/store"
)

type Options struct {
	Pages      int
	Sites      int
	DBPath     string
	ReportPath string
}

type demoCandidate struct {
	ID         string
	ProjectNo  string
	Section    string
	Title      string
	Publish    time.Time
	Body       string
	Href       string
	Status     string
	EmptyTitle bool
}

type attemptKind int

const (
	attemptGood attemptKind = iota
	attemptTimeout
	attemptRefused
	attempt4xx
	attempt5xx
	attemptParsePage
)

type attempt struct {
	Index int
	Site  int
	Kind  attemptKind
	Items []demoCandidate
}

type phaseFixture struct {
	server   *httptest.Server
	attempts []attempt
	refused  string
}

// Run creates an entirely local, deterministic demonstration and executes
// full, incremental and idempotent rerun phases against the same SQLite file.
func Run(ctx context.Context, opts Options) (report.Report, error) {
	if opts.Pages < 60 {
		return report.Report{}, fmt.Errorf("demo requires at least 60 pages")
	}
	if opts.Sites < 1 {
		return report.Report{}, fmt.Errorf("demo requires at least one site")
	}
	if opts.Sites > opts.Pages {
		return report.Report{}, fmt.Errorf("demo requires sites <= pages")
	}
	if opts.DBPath == "" {
		opts.DBPath = "data/crawl.db"
	}
	if err := resetDB(opts.DBPath); err != nil {
		return report.Report{}, err
	}
	st, err := store.Open(opts.DBPath)
	if err != nil {
		return report.Report{}, err
	}
	defer st.Close()

	fullFixture, _, err := newFullFixture(opts.Pages, opts.Sites)
	if err != nil {
		return report.Report{}, err
	}
	defer fullFixture.server.Close()
	fullCfg, err := demoConfig(fullFixture.attempts, opts.Sites, fullFixture.refused, fullFixture.server.URL)
	if err != nil {
		return report.Report{}, err
	}
	fullStats, err := pipeline.New(fullCfg, st).Run(ctx, pipeline.ModeFull)
	if err != nil {
		return report.Report{}, fmt.Errorf("full phase: %w", err)
	}

	incrFixture, err := newIncrementalFixture(opts.Sites)
	if err != nil {
		return report.Report{}, err
	}
	defer incrFixture.server.Close()
	incrCfg, err := demoConfig(incrFixture.attempts, opts.Sites, "", incrFixture.server.URL)
	if err != nil {
		return report.Report{}, err
	}
	incrStats, err := pipeline.New(incrCfg, st).Run(ctx, pipeline.ModeIncremental)
	if err != nil {
		return report.Report{}, fmt.Errorf("incremental phase: %w", err)
	}
	beforeRows := incrStats.Rows
	rerunStats, err := pipeline.New(incrCfg, st).Run(ctx, pipeline.ModeIncremental)
	if err != nil {
		return report.Report{}, fmt.Errorf("rerun phase: %w", err)
	}
	rstat := &report.RerunStats{
		New:        rerunStats.New,
		Revised:    rerunStats.Revised,
		Rows:       rerunStats.Rows,
		BeforeRows: beforeRows,
		Idempotent: rerunStats.New == 0 && rerunStats.Revised == 0 && rerunStats.Rows == beforeRows,
	}
	result := report.Report{Full: &fullStats, Incremental: &incrStats, Rerun: rstat}
	if err := result.WriteJSON(opts.ReportPath); err != nil {
		return report.Report{}, err
	}
	return result, nil
}

func resetDB(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func newFullFixture(pages, sites int) (*phaseFixture, []demoCandidate, error) {
	candidates := buildFullCandidates()
	at := &phaseFixture{}
	at.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idxRaw := strings.TrimPrefix(r.URL.Path, "/attempt/")
		idx, err := strconv.Atoi(idxRaw)
		if err != nil || idx < 0 || idx >= len(at.attempts) {
			http.NotFound(w, r)
			return
		}
		a := at.attempts[idx]
		switch a.Kind {
		case attemptTimeout:
			time.Sleep(250 * time.Millisecond)
			renderGoodPage(w, a.Items, false)
		case attempt4xx:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintln(w, "not found")
		case attempt5xx:
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintln(w, "internal error")
		case attemptParsePage:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintln(w, `<html><body><div class="empty-page">no announcements here</div></body></html>`)
		case attemptRefused:
			http.NotFound(w, r)
		default:
			renderGoodPage(w, a.Items, idx == 33)
		}
	}))
	closedPort, err := closedURLPort()
	if err != nil {
		at.server.Close()
		return nil, nil, err
	}
	at.refused = "http://127.0.0.1:" + closedPort + "/attempt/9"

	kinds := make([]attemptKind, pages)
	for i := 0; i < 9 && i < pages; i++ {
		kinds[i] = attemptTimeout
	}
	if pages > 9 {
		kinds[9] = attemptRefused
	}
	for i := 10; i < 21 && i < pages; i++ {
		kinds[i] = attempt4xx
	}
	for i := 21; i < 23 && i < pages; i++ {
		kinds[i] = attempt5xx
	}
	for i := 23; i < 28 && i < pages; i++ {
		kinds[i] = attemptParsePage
	}
	goodPages := 0
	for _, kind := range kinds {
		if kind == attemptGood {
			goodPages++
		}
	}
	if goodPages == 0 {
		at.server.Close()
		return nil, nil, fmt.Errorf("demo needs at least one good page")
	}

	// 612 raw candidates over 572 good pages: the first 40 pages carry two
	// candidates. Candidate order is independent of page order because the
	// pipeline sorts by (publish_time, business_key).
	assignCount := 0
	candidateIndex := 0
	for i := 0; i < pages; i++ {
		a := attempt{Index: i, Site: i % sites, Kind: kinds[i]}
		if a.Kind == attemptGood {
			n := 1
			if assignCount < 40 {
				n = 2
			}
			assignCount++
			for j := 0; j < n; j++ {
				if candidateIndex < len(candidates) {
					a.Items = append(a.Items, candidates[candidateIndex])
					candidateIndex++
					continue
				}
				filler := candidates[(candidateIndex-len(candidates))%573]
				filler.ID = fmt.Sprintf("%s-filler-%d", filler.ID, candidateIndex)
				filler.Href = fmt.Sprintf("/ann/%s?filler=%d", filler.ProjectNo, candidateIndex)
				filler.Status = ""
				a.Items = append(a.Items, filler)
				candidateIndex++
			}
		}
		at.attempts = append(at.attempts, a)
	}
	return at, candidates, nil
}

func renderGoodPage(w http.ResponseWriter, items []demoCandidate, gbk bool) {
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head>`)
	if gbk {
		b.WriteString(`<meta charset="gbk">`)
	} else {
		b.WriteString(`<meta charset="utf-8">`)
	}
	b.WriteString(`<title>招标公告</title></head><body><ul>`)
	for _, c := range items {
		b.WriteString(`<li class="item"><a class="title" href="`)
		b.WriteString(html.EscapeString(c.Href))
		b.WriteString(`">`)
		if !c.EmptyTitle {
			b.WriteString(html.EscapeString(c.Title))
		}
		b.WriteString(`</a><span class="project">`)
		b.WriteString(html.EscapeString(c.ProjectNo))
		b.WriteString(`</span><span class="section">`)
		b.WriteString(html.EscapeString(c.Section))
		b.WriteString(`</span><span class="status">`)
		b.WriteString(html.EscapeString(c.Status))
		b.WriteString(`</span><time class="publish">`)
		b.WriteString(c.Publish.UTC().Format(time.RFC3339))
		b.WriteString(`</time><div class="body">`)
		b.WriteString(html.EscapeString(c.Body))
		b.WriteString(`</div></li>`)
	}
	b.WriteString(`</ul></body></html>`)
	payload := []byte(b.String())
	if gbk {
		encoded, err := simplifiedchinese.GBK.NewEncoder().Bytes(payload)
		if err == nil {
			payload = encoded
		}
		w.Header().Set("Content-Type", "text/html; charset=gbk")
	} else {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

func buildFullCandidates() []demoCandidate {
	var out []demoCandidate
	baseKeys := make([]string, 0, 573)
	for i := 1; i <= 572; i++ {
		baseKeys = append(baseKeys, fmt.Sprintf("P-%04d", i))
	}
	baseKeys = append(baseKeys, "P-0591")
	for i, key := range baseKeys {
		publish := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second)
		// The final key is the deliberate global watermark boundary.
		if key == "P-0591" {
			publish = time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
		}
		body := "项目 " + key + " 招标正文版本一"
		if key == "P-0001" {
			body = strings.Repeat("很长的招标正文用于验证截断边界。", 400)
		}
		out = append(out, demoCandidate{
			ID: key, ProjectNo: key, Title: "招标公告 " + key,
			Publish: publish, Body: body,
			Href: "/ann/" + key,
		})
	}
	for i := 1; i <= 17; i++ {
		key := fmt.Sprintf("P-%04d", i)
		status := "active"
		body := "项目 " + key + " 招标正文版本二，已修订"
		if i <= 2 {
			status = "withdrawn"
			body = "项目 " + key + " 招标已撤稿"
		}
		out = append(out, demoCandidate{
			ID: key + "-rev", ProjectNo: key,
			Title:   "招标公告 " + key + "（修订）",
			Publish: time.Date(2026, 11, 12, 9, 29, 31+i, 0, time.UTC),
			Body:    body, Status: status,
			Href: "/ann/" + key + "?revision=2",
		})
	}
	for i := 1; i <= 21; i++ {
		key := fmt.Sprintf("P-%04d", 499+i)
		publish := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC).Add(time.Duration(498+i) * time.Second)
		out = append(out, demoCandidate{
			ID: key + "-dup", ProjectNo: key, Title: "招标公告 " + key,
			Publish: publish, Body: "项目 " + key + " 招标正文版本一",
			Href: "/ann/" + key + "?utm_source=demo" + strconv.Itoa(i),
		})
	}
	out = append(out, demoCandidate{
		ID: "bad", ProjectNo: "P-9999", Title: "",
		Publish:    time.Date(2026, 11, 12, 9, 29, 59, 0, time.UTC),
		Body:       "坏标题候选",
		Href:       "/ann/P-9999?invalid=1",
		EmptyTitle: true,
	})
	return out
}

func newIncrementalFixture(sites int) (*phaseFixture, error) {
	at := &phaseFixture{}
	items := buildIncrementalCandidates()
	for i, item := range items {
		at.attempts = append(at.attempts, attempt{
			Index: i, Site: i % sites, Kind: attemptGood,
			Items: []demoCandidate{item},
		})
	}
	at.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idxRaw := strings.TrimPrefix(r.URL.Path, "/attempt/")
		idx, err := strconv.Atoi(idxRaw)
		if err != nil || idx < 0 || idx >= len(at.attempts) {
			http.NotFound(w, r)
			return
		}
		renderGoodPage(w, at.attempts[idx].Items, false)
	}))
	return at, nil
}

func buildIncrementalCandidates() []demoCandidate {
	var out []demoCandidate
	// 115 unchanged forwards: same business key and same fingerprint => dedup.
	for i := 0; i < 115; i++ {
		key := fmt.Sprintf("P-%04d", 100+i)
		publish := time.Date(2026, 11, 12, 9, 5, 0, 0, time.UTC).Add(time.Duration(i) * time.Second)
		out = append(out, demoCandidate{
			ID: key + "-fwd", ProjectNo: key, Title: "招标公告 " + key,
			Publish: publish, Body: "项目 " + key + " 招标正文版本一",
			Href: "/ann/" + key + "?forward=" + strconv.Itoa(i),
		})
	}
	// 3 changed versions under existing keys => revisions.
	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("P-%04d", 300+i)
		out = append(out, demoCandidate{
			ID: key + "-rev2", ProjectNo: key, Title: "招标公告 " + key + "（二次修订）",
			Publish: time.Date(2026, 11, 12, 9, 20+i, 0, 0, time.UTC),
			Body:    "项目 " + key + " 招标正文版本三",
			Href:    "/ann/" + key + "?revision=3",
		})
	}
	// 22 genuinely new business keys.
	for i := 0; i < 22; i++ {
		key := fmt.Sprintf("P-%04d", 800+i)
		out = append(out, demoCandidate{
			ID: key, ProjectNo: key, Title: "招标公告 " + key,
			Publish: time.Date(2026, 11, 12, 9, 10, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute),
			Body:    "项目 " + key + " 招标正文首版",
			Href:    "/ann/" + key,
		})
	}
	return out
}

func demoConfig(attempts []attempt, sites int, refused string, baseURL string) (*config.Config, error) {
	cfg := &config.Config{}
	for site := 0; site < sites; site++ {
		var starts []string
		for _, a := range attempts {
			if a.Site != site {
				continue
			}
			if a.Kind == attemptRefused && refused != "" {
				starts = append(starts, refused)
				continue
			}
			starts = append(starts, fmt.Sprintf("%s/attempt/%d", baseURL, a.Index))
		}
		cfg.Sites = append(cfg.Sites, config.Site{
			Name:          fmt.Sprintf("site-%d", site+1),
			StartURLs:     starts,
			Concurrency:   3,
			RatePerSecond: 5000,
			Burst:         20,
			Timeout:       config.Duration(50 * time.Millisecond),
			Retries:       0,
			BreakAfter5xx: 20,
			Pause:         config.Duration(time.Second),
			MaxPages:      2000,
			MaxBodyRunes:  1200,
			TimeLayouts:   []string{time.RFC3339},
			Selectors: config.Selectors{
				Item: ".item", Title: ".title", Link: ".title",
				ProjectNo: ".project", Section: ".section",
				PublishTime: ".publish", Body: ".body", Status: ".status",
			},
		})
	}
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func closedURLPort() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := listener.Addr().(*net.TCPAddr)
	port := addr.Port
	listener.Close()
	return strconv.Itoa(port), nil
}
