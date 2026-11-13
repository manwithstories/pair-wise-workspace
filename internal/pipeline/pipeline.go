package pipeline

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"crawler/internal/canon"
	"crawler/internal/config"
	"crawler/internal/extract"
	"crawler/internal/fetch"
	"crawler/internal/report"
	"crawler/internal/store"
)

type Mode string

const (
	ModeFull        Mode = "full"
	ModeIncremental Mode = "incr"
)

type Pipeline struct {
	cfg      *config.Config
	store    *store.Store
	fetchers map[string]*fetch.Fetcher
}

func New(cfg *config.Config, st *store.Store) *Pipeline {
	fetchers := make(map[string]*fetch.Fetcher, len(cfg.Sites))
	for _, site := range cfg.Sites {
		fetchers[site.Name] = fetch.New(site)
	}
	return &Pipeline{cfg: cfg, store: st, fetchers: fetchers}
}

type runState struct {
	mu       sync.Mutex
	fetched  int
	rawURL   int
	uniq     map[string]struct{}
	records  []store.Record
	failures []store.Failure
	failureN map[string]int
	runID    string
	mode     string
}

func newRunState(runID, mode string) *runState {
	return &runState{
		uniq:     make(map[string]struct{}),
		failureN: make(map[string]int),
		runID:    runID,
		mode:     mode,
	}
}

func (s *runState) addRecord(r store.Record) {
	s.mu.Lock()
	s.records = append(s.records, r)
	s.mu.Unlock()
}

func (s *runState) addFailure(f store.Failure) {
	s.mu.Lock()
	s.failures = append(s.failures, f)
	s.failureN[f.Class]++
	s.mu.Unlock()
}

func (s *runState) incFetched() {
	s.mu.Lock()
	s.fetched++
	s.mu.Unlock()
}

func (s *runState) addRaw() {
	s.mu.Lock()
	s.rawURL++
	s.mu.Unlock()
}

func (s *runState) addUniq(canonical string) {
	s.mu.Lock()
	s.uniq[canonical] = struct{}{}
	s.mu.Unlock()
}

func (p *Pipeline) Run(ctx context.Context, mode Mode) (report.RunStats, error) {
	if mode != ModeFull && mode != ModeIncremental {
		return report.RunStats{}, fmt.Errorf("unknown mode %q", mode)
	}
	started := time.Now().UTC()
	runID := started.Format("20060102T150405.000000000") + "-" + string(mode)
	state := newRunState(runID, string(mode))

	watermark, err := p.store.ReadWatermark(ctx, "incr")
	if err != nil && !errorsIsNoWatermark(err) {
		return report.RunStats{}, err
	}
	lower := time.Time{}
	if mode == ModeIncremental && !watermark.IsZero() {
		lower = watermark.PublishTime.Add(-30 * time.Minute)
	}
	window := lookbackWindow{Lower: lower, Watermark: watermark}

	var wg sync.WaitGroup
	errCh := make(chan error, len(p.cfg.Sites))
	for i := range p.cfg.Sites {
		site := &p.cfg.Sites[i]
		fetcher := p.fetchers[site.Name]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.crawlSite(ctx, site, fetcher, state, mode, window); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	if err := ctx.Err(); err != nil {
		return report.RunStats{}, fmt.Errorf("crawl interrupted before commit: %w", err)
	}
	for err := range errCh {
		if err != nil {
			return report.RunStats{}, err
		}
	}

	records := append([]store.Record(nil), state.records...)
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].PublishTime.Equal(records[j].PublishTime) {
			return records[i].PublishTime.Before(records[j].PublishTime)
		}
		if records[i].BusinessKey != records[j].BusinessKey {
			return records[i].BusinessKey < records[j].BusinessKey
		}
		return records[i].RawURL < records[j].RawURL
	})
	applyResult, err := p.store.ApplyBatch(ctx, records, store.ApplyOptions{
		Mode:             string(mode),
		AdvanceWatermark: true,
	})
	if err != nil {
		return report.RunStats{}, err
	}
	if err := p.store.RecordFailures(ctx, state.failures); err != nil {
		return report.RunStats{}, err
	}

	stats := report.RunStats{
		RunID:      runID,
		Mode:       string(mode),
		Fetched:    state.fetched,
		RawURL:     state.rawURL,
		UniqURL:    len(state.uniq),
		Dedup:      applyResult.Duplicate,
		New:        applyResult.New,
		Revised:    applyResult.Revised,
		Failed:     len(state.failures),
		Failures:   state.failureN,
		Rows:       applyResult.Rows,
		StartedAt:  started,
		FinishedAt: time.Now().UTC(),
	}
	if stats.Failures == nil {
		stats.Failures = map[string]int{}
	}
	wm, err := p.store.ReadWatermark(ctx, "incr")
	if err != nil && !errorsIsNoWatermark(err) {
		return report.RunStats{}, err
	}
	if !wm.IsZero() {
		stats.Watermark = wm.String()
	}
	if err := p.store.SaveRun(ctx, store.RunRecord{
		ID: runID, Mode: stats.Mode, Fetched: stats.Fetched, RawURL: stats.RawURL,
		UniqURL: stats.UniqURL, Dedup: stats.Dedup, New: stats.New, Revised: stats.Revised,
		Failed: stats.Failed, Failures: stats.Failures, Watermark: stats.Watermark,
		Rows: stats.Rows, StartedAt: stats.StartedAt, FinishedAt: stats.FinishedAt,
	}); err != nil {
		return report.RunStats{}, err
	}
	return stats, nil
}

type lookbackWindow struct {
	Lower     time.Time
	Watermark store.Watermark
}

type visitedSet struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (v *visitedSet) add(key string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.seen[key] {
		return false
	}
	v.seen[key] = true
	return true
}

func (p *Pipeline) crawlSite(ctx context.Context, site *config.Site, fetcher *fetch.Fetcher, state *runState, mode Mode, window lookbackWindow) error {
	visited := &visitedSet{seen: make(map[string]bool)}
	var starts []string
	for _, start := range site.StartURLs {
		canonical, err := canon.CanonicalizeURL(start, nil, site.RemoveQueryParams)
		if err != nil {
			return fmt.Errorf("site %s start URL %q: %w", site.Name, start, err)
		}
		if visited.add(canonical) {
			starts = append(starts, canonical)
		}
	}
	var pageCount atomic.Int64
	concurrency := site.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	errCh := make(chan error, len(starts))
	var wg sync.WaitGroup
	for _, start := range starts {
		wg.Add(1)
		go func(start string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
			if err := p.crawlChain(ctx, site, fetcher, state, mode, window, visited, &pageCount, start); err != nil {
				errCh <- err
			}
		}(start)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Pipeline) crawlChain(ctx context.Context, site *config.Site, fetcher *fetch.Fetcher, state *runState, mode Mode, window lookbackWindow, visited *visitedSet, pageCount *atomic.Int64, start string) error {
	current := start
	for current != "" {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if pageCount.Add(1) > int64(site.MaxPages) {
			return nil
		}

		state.incFetched()
		resp := fetcher.Fetch(ctx, current)
		if !resp.OK() {
			class := resp.Class
			if class == fetch.ClassNone {
				class = fetch.ClassNetwork
			}
			message := "fetch failed"
			if resp.Err != nil {
				message = resp.Err.Error()
			}
			state.addFailure(store.Failure{
				RunID: state.runID, Mode: state.mode, Site: site.Name, URL: current,
				Kind: "page", Class: string(class), StatusCode: resp.Status, Message: message,
				OccurredAt: time.Now().UTC(),
			})
			break
		}

		pageBase := current
		if resp.FinalURL != "" {
			pageBase = resp.FinalURL
		}
		baseURL, err := url.Parse(pageBase)
		if err != nil {
			return fmt.Errorf("site %s final URL %q: %w", site.Name, pageBase, err)
		}
		page, err := extract.ParsePage(site, pageBase, resp.Body, resp.ContentType)
		if err != nil {
			state.addFailure(store.Failure{
				RunID: state.runID, Mode: state.mode, Site: site.Name, URL: current,
				Kind: "page", Class: string(fetch.ClassParse), StatusCode: resp.Status, Message: err.Error(),
				OccurredAt: time.Now().UTC(),
			})
			break
		}

		allOld := len(page.Candidates) > 0
		var details []detailJob
		for i := range page.Candidates {
			candidate := &page.Candidates[i]
			state.addRaw()
			canonicalURL, resolveErr := canon.CanonicalizeURL(candidate.RawURL, baseURL, site.RemoveQueryParams)
			if resolveErr != nil {
				state.addFailure(store.Failure{
					RunID: state.runID, Mode: state.mode, Site: site.Name, URL: candidate.RawURL,
					Kind: "entry", Class: string(fetch.ClassParse), StatusCode: resp.Status, Message: resolveErr.Error(),
					OccurredAt: time.Now().UTC(),
				})
				allOld = false
				continue
			}
			state.addUniq(canonicalURL)
			if candidate.ParseError != nil {
				state.addFailure(store.Failure{
					RunID: state.runID, Mode: state.mode, Site: site.Name, URL: canonicalURL,
					Kind: "entry", Class: string(fetch.ClassParse), StatusCode: resp.Status, Message: candidate.ParseError.Error(),
					OccurredAt: time.Now().UTC(),
				})
				if candidate.PublishTime.IsZero() || withinWindow(candidate.PublishTime, canon.BusinessKey(candidate.ProjectNo, candidate.Section), window) {
					allOld = false
				}
				continue
			}
			if site.Detail {
				allOld = false
				details = append(details, detailJob{candidate: candidate, canonicalURL: canonicalURL})
				continue
			}
			if mode == ModeIncremental && !withinWindow(candidate.PublishTime, canon.BusinessKey(candidate.ProjectNo, candidate.Section), window) {
				continue
			}
			allOld = false
			state.addRecord(makeRecord(site, candidate, canonicalURL))
		}
		if mode == ModeIncremental && window.Lower != (time.Time{}) && allOld {
			break
		}

		if len(details) > 0 {
			var detailWG sync.WaitGroup
			for _, job := range details {
				detailWG.Add(1)
				go func(job detailJob) {
					defer detailWG.Done()
					p.processDetail(ctx, site, fetcher, state, job.candidate, job.canonicalURL, mode, window)
				}(job)
			}
			detailWG.Wait()
		}

		nextRaw := strings.TrimSpace(page.NextRaw)
		if nextRaw == "" {
			break
		}
		nextCanonical, err := canon.CanonicalizeURL(nextRaw, baseURL, site.RemoveQueryParams)
		if err != nil {
			state.addFailure(store.Failure{
				RunID: state.runID, Mode: state.mode, Site: site.Name, URL: nextRaw,
				Kind: "next", Class: string(fetch.ClassParse), StatusCode: resp.Status, Message: err.Error(),
				OccurredAt: time.Now().UTC(),
			})
			break
		}
		if nextCanonical == current || !visited.add(nextCanonical) {
			break
		}
		pageNum := page.PageNum
		if pageNum == 0 {
			pageNum = pageNumFromURL(current)
		}
		nextNum := pageNumFromURL(nextCanonical)
		if nextNum > 0 && pageNum > 0 && nextNum <= pageNum {
			break
		}
		current = nextCanonical
	}
	return nil
}

type detailJob struct {
	candidate    *extract.Candidate
	canonicalURL string
}

func normalizeStatus(raw string) string {
	status := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case strings.Contains(status, "撤稿"), strings.Contains(status, "withdraw"),
		strings.Contains(status, "terminated"), strings.Contains(status, "cancelled"):
		return "withdrawn"
	default:
		return "active"
	}
}

func withinWindow(publish time.Time, businessKey string, window lookbackWindow) bool {
	if window.Watermark.IsZero() {
		return true
	}
	if publish.After(window.Watermark.PublishTime) {
		return true
	}
	if publish.Equal(window.Watermark.PublishTime) && businessKey > window.Watermark.BusinessKey {
		return true
	}
	return !publish.Before(window.Lower)
}

func pageNumFromURL(raw string) int {
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	q := u.Query()
	for _, key := range []string{"page", "pn", "pageNo", "page_no", "p"} {
		if rawValue := q.Get(key); rawValue != "" {
			if n, err := strconv.Atoi(rawValue); err == nil {
				return n
			}
		}
	}
	return 0
}

func (p *Pipeline) processDetail(ctx context.Context, site *config.Site, fetcher *fetch.Fetcher, state *runState, candidate *extract.Candidate, canonicalURL string, mode Mode, window lookbackWindow) {
	state.incFetched()
	resp := fetcher.Fetch(ctx, canonicalURL)
	if !resp.OK() {
		class := resp.Class
		if class == fetch.ClassNone {
			class = fetch.ClassNetwork
		}
		message := "detail fetch failed"
		if resp.Err != nil {
			message = resp.Err.Error()
		}
		state.addFailure(store.Failure{
			RunID: state.runID, Mode: state.mode, Site: site.Name, URL: canonicalURL,
			Kind: "detail", Class: string(class), StatusCode: resp.Status, Message: message,
			OccurredAt: time.Now().UTC(),
		})
		return
	}
	detail, err := extract.ParseDetail(site, canonicalURL, resp.Body, resp.ContentType)
	if err != nil {
		state.addFailure(store.Failure{
			RunID: state.runID, Mode: state.mode, Site: site.Name, URL: canonicalURL,
			Kind: "detail", Class: string(fetch.ClassParse), StatusCode: resp.Status, Message: err.Error(),
			OccurredAt: time.Now().UTC(),
		})
		return
	}
	if mode == ModeIncremental && !withinWindow(detail.PublishTime, canon.BusinessKey(detail.ProjectNo, detail.Section), window) {
		return
	}
	state.addRecord(makeRecord(site, detail, canonicalURL))
}

func makeRecord(site *config.Site, c *extract.Candidate, canonicalURL string) store.Record {
	title := strings.TrimSpace(c.Title)
	return store.Record{
		RawURL:       c.RawURL,
		CanonicalURL: canonicalURL,
		Site:         site.Name,
		Title:        title,
		TitleNorm:    canon.NormalizeTitle(title),
		ProjectNo:    strings.TrimSpace(c.ProjectNo),
		Section:      strings.TrimSpace(c.Section),
		BusinessKey:  canon.BusinessKey(c.ProjectNo, c.Section),
		PublishTime:  c.PublishTime.UTC(),
		Body:         canon.TruncateRunes(canon.NormalizeBody(c.Body), site.MaxBodyRunes),
		ContentHash:  canon.Fingerprint(title, c.Body, c.ProjectNo, c.Section),
		Status:       normalizeStatus(c.Status),
	}
}

func errorsIsNoWatermark(err error) bool {
	return err == store.ErrNoWatermark || (err != nil && strings.Contains(err.Error(), store.ErrNoWatermark.Error()))
}

// Replay processes unresolved failures of one class.
func (p *Pipeline) Replay(ctx context.Context, class string) (report.RunStats, error) {
	started := time.Now().UTC()
	runID := started.Format("20060102T150405.000000000") + "-replay"
	state := newRunState(runID, "replay")
	failures, err := p.store.UnresolvedFailures(ctx, class)
	if err != nil {
		return report.RunStats{}, err
	}
	resolvedURLs := make([]string, 0, len(failures))
	for _, failure := range failures {
		site, ok := p.cfg.Site(failure.Site)
		if !ok {
			continue
		}
		fetcher := p.fetchers[site.Name]
		state.incFetched()
		resp := fetcher.Fetch(ctx, failure.URL)
		if !resp.OK() {
			newClass := resp.Class
			if newClass == fetch.ClassNone {
				newClass = fetch.ClassNetwork
			}
			msg := "replay fetch failed"
			if resp.Err != nil {
				msg = resp.Err.Error()
			}
			state.addFailure(store.Failure{
				RunID: runID, Mode: "replay", Site: site.Name, URL: failure.URL,
				Kind: failure.Kind, Class: string(newClass), StatusCode: resp.Status, Message: msg,
				OccurredAt: time.Now().UTC(),
			})
			continue
		}
		if failure.Kind == "entry" || failure.Kind == "detail" {
			record, parseErr := replayDetail(site, failure.URL, resp)
			if parseErr != nil {
				state.addFailure(store.Failure{
					RunID: runID, Mode: "replay", Site: site.Name, URL: failure.URL,
					Kind: failure.Kind, Class: string(fetch.ClassParse), StatusCode: resp.Status, Message: parseErr.Error(),
					OccurredAt: time.Now().UTC(),
				})
				continue
			}
			state.addRaw()
			state.addUniq(failure.URL)
			state.addRecord(record)
			resolvedURLs = append(resolvedURLs, failure.URL)
			continue
		}
		page, err := extract.ParsePage(site, failure.URL, resp.Body, resp.ContentType)
		if err != nil {
			state.addFailure(store.Failure{
				RunID: runID, Mode: "replay", Site: site.Name, URL: failure.URL,
				Kind: failure.Kind, Class: string(fetch.ClassParse), StatusCode: resp.Status, Message: err.Error(),
				OccurredAt: time.Now().UTC(),
			})
			continue
		}
		baseURL, _ := url.Parse(failure.URL)
		for i := range page.Candidates {
			c := &page.Candidates[i]
			state.addRaw()
			canonicalURL, resolveErr := canon.CanonicalizeURL(c.RawURL, baseURL, site.RemoveQueryParams)
			if resolveErr != nil || c.ParseError != nil {
				msg := "candidate parse failed"
				if resolveErr != nil {
					msg = resolveErr.Error()
				} else if c.ParseError != nil {
					msg = c.ParseError.Error()
				}
				state.addFailure(store.Failure{
					RunID: runID, Mode: "replay", Site: site.Name, URL: c.RawURL,
					Kind: "entry", Class: string(fetch.ClassParse), StatusCode: resp.Status, Message: msg,
					OccurredAt: time.Now().UTC(),
				})
				continue
			}
			state.addUniq(canonicalURL)
			state.addRecord(makeRecord(site, c, canonicalURL))
			resolvedURLs = append(resolvedURLs, canonicalURL)
		}
		resolvedURLs = append(resolvedURLs, failure.URL)
	}
	records := append([]store.Record(nil), state.records...)
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].PublishTime.Equal(records[j].PublishTime) {
			return records[i].PublishTime.Before(records[j].PublishTime)
		}
		return records[i].BusinessKey < records[j].BusinessKey
	})
	applyResult, err := p.store.ApplyBatch(ctx, records, store.ApplyOptions{Mode: string(ModeIncremental), AdvanceWatermark: true})
	if err != nil {
		return report.RunStats{}, err
	}
	if err := p.store.RecordFailures(ctx, state.failures); err != nil {
		return report.RunStats{}, err
	}
	if err := p.store.ResolveFailures(ctx, resolvedURLs); err != nil {
		return report.RunStats{}, err
	}
	if state.failureN == nil {
		state.failureN = map[string]int{}
	}
	stats := report.RunStats{
		RunID: runID, Mode: "replay", Fetched: state.fetched, RawURL: state.rawURL, UniqURL: len(state.uniq),
		Dedup: applyResult.Duplicate, New: applyResult.New, Revised: applyResult.Revised,
		Failed: len(state.failures), Failures: state.failureN, Rows: applyResult.Rows,
		StartedAt: started, FinishedAt: time.Now().UTC(),
	}
	wm, err := p.store.ReadWatermark(ctx, "incr")
	if err != nil && !errorsIsNoWatermark(err) {
		return report.RunStats{}, err
	}
	if !wm.IsZero() {
		stats.Watermark = wm.String()
	}
	if err := p.store.SaveRun(ctx, store.RunRecord{
		ID: runID, Mode: stats.Mode, Fetched: stats.Fetched, RawURL: stats.RawURL,
		UniqURL: stats.UniqURL, Dedup: stats.Dedup, New: stats.New, Revised: stats.Revised,
		Failed: stats.Failed, Failures: stats.Failures, Watermark: stats.Watermark,
		Rows: stats.Rows, StartedAt: stats.StartedAt, FinishedAt: stats.FinishedAt,
	}); err != nil {
		return report.RunStats{}, err
	}
	return stats, nil
}

func replayDetail(site *config.Site, rawURL string, resp fetch.Response) (store.Record, error) {
	detail, err := extract.ParseDetail(site, rawURL, resp.Body, resp.ContentType)
	if err != nil {
		return store.Record{}, err
	}
	return makeRecord(site, detail, rawURL), nil
}
