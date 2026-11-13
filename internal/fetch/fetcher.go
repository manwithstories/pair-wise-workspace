package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gocolly/colly/v2"

	"crawler/internal/config"
)

type Class string

const (
	ClassNone        Class = ""
	ClassTimeout     Class = "timeout"
	ClassRefused     Class = "refused"
	ClassHTTP4xx     Class = "http_4xx"
	ClassHTTP5xx     Class = "http_5xx"
	ClassRateLimited Class = "rate_limited"
	ClassNetwork     Class = "network"
	ClassParse       Class = "parse"
)

type Response struct {
	RequestedURL string
	FinalURL     string
	Status       int
	Body         []byte
	ContentType  string
	Err          error
	Class        Class
	StatusCode   int
	Duration     time.Duration
}

func (r Response) OK() bool {
	return r.Err == nil && r.Status >= 200 && r.Status < 300 && r.Class == ClassNone
}

// Fetcher owns a site's Colly policy, token bucket, concurrency limit and
// consecutive-5xx circuit breaker.
type Fetcher struct {
	site   config.Site
	sem    chan struct{}
	bucket *tokenBucket
	mu     sync.Mutex
	fivexx int
	paused time.Time
}

func New(site config.Site) *Fetcher {
	concurrency := site.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	return &Fetcher{
		site:   site,
		sem:    make(chan struct{}, concurrency),
		bucket: newTokenBucket(site.RatePerSecond, site.Burst),
	}
}

func (f *Fetcher) Site() config.Site { return f.site }

func (f *Fetcher) Fetch(ctx context.Context, rawURL string) Response {
	select {
	case f.sem <- struct{}{}:
		defer func() { <-f.sem }()
	case <-ctx.Done():
		return Response{RequestedURL: rawURL, Err: ctx.Err(), Class: ClassTimeout}
	}
	if err := f.waitForPause(ctx); err != nil {
		return Response{RequestedURL: rawURL, Err: err, Class: ClassTimeout}
	}

	var last Response
	attempts := f.site.Retries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			delay := f.site.RetryBackoff.Std() * time.Duration(1<<(attempt-1))
			if delay > 5*time.Second {
				delay = 5 * time.Second
			}
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return Response{RequestedURL: rawURL, Err: ctx.Err(), Class: ClassTimeout}
			}
			if err := f.bucket.Wait(ctx); err != nil {
				return Response{RequestedURL: rawURL, Err: err, Class: ClassTimeout}
			}
		} else {
			if err := f.bucket.Wait(ctx); err != nil {
				return Response{RequestedURL: rawURL, Err: err, Class: ClassTimeout}
			}
		}
		last = f.fetchOnce(ctx, rawURL)
		if last.Class != ClassTimeout && last.Class != ClassRefused && last.Class != ClassNetwork {
			return last
		}
	}
	return last
}

func (f *Fetcher) fetchOnce(ctx context.Context, rawURL string) Response {
	started := time.Now()
	resp := Response{RequestedURL: rawURL}
	c := colly.NewCollector()
	c.AllowURLRevisit = true
	c.DetectCharset = false
	c.MaxBodySize = 4 << 20
	c.SetRequestTimeout(f.site.Timeout.Std())
	c.UserAgent = f.site.UserAgent
	headers := make(map[string]string, len(f.site.Headers))
	for key, value := range f.site.Headers {
		headers[key] = value
	}
	c.OnRequest(func(r *colly.Request) {
		for key, value := range headers {
			r.Headers.Set(key, value)
		}
	})
	c.OnResponse(func(r *colly.Response) {
		resp.FinalURL = r.Request.URL.String()
		resp.Status = r.StatusCode
		resp.StatusCode = r.StatusCode
		resp.Body = append([]byte(nil), r.Body...)
		resp.ContentType = r.Headers.Get("Content-Type")
	})
	c.OnError(func(r *colly.Response, err error) {
		if r != nil {
			if resp.FinalURL == "" && r.Request != nil {
				resp.FinalURL = r.Request.URL.String()
			}
			resp.Status = r.StatusCode
			resp.StatusCode = r.StatusCode
			if len(r.Body) > 0 {
				resp.Body = append([]byte(nil), r.Body...)
			}
			if r.Headers != nil {
				resp.ContentType = r.Headers.Get("Content-Type")
			}
		}
		resp.Err = err
	})
	err := c.Visit(rawURL)
	if resp.Err == nil {
		resp.Err = err
	}
	resp.Duration = time.Since(started)
	resp.Class = classify(resp.Err, resp.Status)
	f.observe(resp.Class)
	return resp
}

func classify(err error, status int) Class {
	switch {
	case status == 429:
		return ClassRateLimited
	case status >= 400 && status < 500:
		return ClassHTTP4xx
	case status >= 500:
		return ClassHTTP5xx
	case status >= 400:
		return ClassHTTP4xx
	}
	if err == nil {
		return ClassNone
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ClassTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ClassTimeout
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) {
		return ClassRefused
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Op == "dial" {
			return ClassRefused
		}
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded"):
		return ClassTimeout
	case strings.Contains(lower, "connection refused"):
		return ClassRefused
	default:
		return ClassNetwork
	}
}

func (f *Fetcher) observe(class Class) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if class != ClassHTTP5xx {
		f.fivexx = 0
		return
	}
	f.fivexx++
	if f.site.BreakAfter5xx > 0 && f.fivexx >= f.site.BreakAfter5xx {
		f.paused = time.Now().Add(f.site.Pause.Std())
		f.fivexx = 0
	}
}

func (f *Fetcher) waitForPause(ctx context.Context) error {
	f.mu.Lock()
	until := f.paused
	f.mu.Unlock()
	if until.IsZero() || time.Now().After(until) {
		return nil
	}
	timer := time.NewTimer(time.Until(until))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type tokenBucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newTokenBucket(ratePerSecond float64, burst int) *tokenBucket {
	if ratePerSecond <= 0 {
		ratePerSecond = 1
	}
	if burst < 1 {
		burst = 1
	}
	return &tokenBucket{rate: ratePerSecond, burst: float64(burst), tokens: float64(burst), last: time.Now()}
}

func (b *tokenBucket) Wait(ctx context.Context) error {
	for {
		b.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(b.last).Seconds()
		b.tokens += elapsed * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
		if b.tokens >= 1 {
			b.tokens--
			b.mu.Unlock()
			return nil
		}
		wait := time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
		b.mu.Unlock()
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// ValidateURL provides a useful error for malformed URLs before Colly sees
// them.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
	return nil
}
