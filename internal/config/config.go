package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration accepts either a Go duration string ("2s") or an integer number
// of seconds in YAML.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err == nil {
		parsed, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", raw, err)
		}
		*d = Duration(parsed)
		return nil
	}
	var seconds int64
	if err := node.Decode(&seconds); err != nil {
		return fmt.Errorf("duration must be a string or seconds: %w", err)
	}
	*d = Duration(time.Duration(seconds) * time.Second)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) MarshalYAML() (interface{}, error) { return time.Duration(d).String(), nil }

type Selectors struct {
	Item        string `yaml:"item" json:"item"`
	Title       string `yaml:"title" json:"title"`
	Link        string `yaml:"link" json:"link"`
	ProjectNo   string `yaml:"project_no" json:"project_no"`
	Section     string `yaml:"section" json:"section"`
	PublishTime string `yaml:"publish_time" json:"publish_time"`
	Body        string `yaml:"body" json:"body"`
	Status      string `yaml:"status" json:"status"`
	Next        string `yaml:"next" json:"next"`
	PageNum     string `yaml:"page_num" json:"page_num"`
}

type Site struct {
	Name              string            `yaml:"name" json:"name"`
	StartURLs         []string          `yaml:"start_urls" json:"start_urls"`
	Selectors         Selectors         `yaml:"selectors" json:"selectors"`
	Detail            bool              `yaml:"detail" json:"detail"`
	Concurrency       int               `yaml:"concurrency" json:"concurrency"`
	RatePerSecond     float64           `yaml:"rate_per_second" json:"rate_per_second"`
	Burst             int               `yaml:"burst" json:"burst"`
	Timeout           Duration          `yaml:"timeout" json:"timeout"`
	Retries           int               `yaml:"retries" json:"retries"`
	RetryBackoff      Duration          `yaml:"retry_backoff" json:"retry_backoff"`
	BreakAfter5xx     int               `yaml:"break_after_5xx" json:"break_after_5xx"`
	Pause             Duration          `yaml:"pause" json:"pause"`
	MaxPages          int               `yaml:"max_pages" json:"max_pages"`
	MaxBodyRunes      int               `yaml:"max_body_runes" json:"max_body_runes"`
	TimeLayouts       []string          `yaml:"time_layouts" json:"time_layouts"`
	RemoveQueryParams []string          `yaml:"remove_query_params" json:"remove_query_params"`
	UserAgent         string            `yaml:"user_agent" json:"user_agent"`
	Headers           map[string]string `yaml:"headers" json:"headers"`
}

type Config struct {
	Defaults Site              `yaml:"defaults" json:"defaults"`
	Sites    []Site            `yaml:"sites" json:"sites"`
	Meta     map[string]string `yaml:"meta,omitempty" json:"meta,omitempty"`
}

var defaultTimeLayouts = []string{
	time.RFC3339,
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006/01/02 15:04:05",
	"2006-01-02 15:04",
	"2006年01月02日 15:04:05",
	"2006年01月02日",
	"2006-01-02",
}

var defaultTrackingParams = []string{
	"utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content",
	"spm", "from", "ref", "referrer", "_t", "timestamp",
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Normalize merges defaults into every site and validates the result.
func (c *Config) Normalize() error {
	if len(c.Sites) == 0 {
		return fmt.Errorf("config has no sites")
	}
	seen := make(map[string]bool, len(c.Sites))
	for i := range c.Sites {
		s := &c.Sites[i]
		applyDefaults(s, &c.Defaults)
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("site[%d]: name is required", i)
		}
		if seen[s.Name] {
			return fmt.Errorf("duplicate site name %q", s.Name)
		}
		seen[s.Name] = true
		if len(s.StartURLs) == 0 {
			return fmt.Errorf("site %q: at least one start_url is required", s.Name)
		}
		for _, raw := range s.StartURLs {
			u, err := url.Parse(raw)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("site %q: invalid start_url %q", s.Name, raw)
			}
		}
		if s.Selectors.Item == "" {
			return fmt.Errorf("site %q: selectors.item is required", s.Name)
		}
		if s.Selectors.Title == "" {
			return fmt.Errorf("site %q: selectors.title is required", s.Name)
		}
		if s.Concurrency < 1 || s.RatePerSecond <= 0 || s.Burst < 1 {
			return fmt.Errorf("site %q: concurrency, rate_per_second and burst must be positive", s.Name)
		}
		if s.Timeout.Std() <= 0 || s.MaxPages < 1 || s.MaxBodyRunes < 1 {
			return fmt.Errorf("site %q: timeout, max_pages and max_body_runes must be positive", s.Name)
		}
	}
	return nil
}

func applyDefaults(site *Site, def *Site) {
	if site.Concurrency == 0 {
		site.Concurrency = firstNonZero(def.Concurrency, 2)
	}
	if site.RatePerSecond == 0 {
		site.RatePerSecond = def.RatePerSecond
		if site.RatePerSecond == 0 {
			site.RatePerSecond = 5
		}
	}
	if site.Burst == 0 {
		site.Burst = firstNonZero(def.Burst, site.Concurrency)
	}
	if site.Timeout == 0 {
		site.Timeout = def.Timeout
		if site.Timeout == 0 {
			site.Timeout = Duration(10 * time.Second)
		}
	}
	if site.Retries == 0 {
		site.Retries = def.Retries
	}
	if site.RetryBackoff == 0 {
		site.RetryBackoff = def.RetryBackoff
		if site.RetryBackoff == 0 {
			site.RetryBackoff = Duration(250 * time.Millisecond)
		}
	}
	if site.BreakAfter5xx == 0 {
		site.BreakAfter5xx = def.BreakAfter5xx
		if site.BreakAfter5xx == 0 {
			site.BreakAfter5xx = 5
		}
	}
	if site.Pause == 0 {
		site.Pause = def.Pause
		if site.Pause == 0 {
			site.Pause = Duration(5 * time.Second)
		}
	}
	if site.MaxPages == 0 {
		site.MaxPages = firstNonZero(def.MaxPages, 100)
	}
	if site.MaxBodyRunes == 0 {
		site.MaxBodyRunes = firstNonZero(def.MaxBodyRunes, 100000)
	}
	if len(site.TimeLayouts) == 0 {
		site.TimeLayouts = append([]string(nil), def.TimeLayouts...)
		if len(site.TimeLayouts) == 0 {
			site.TimeLayouts = append([]string(nil), defaultTimeLayouts...)
		}
	}
	if len(site.RemoveQueryParams) == 0 {
		site.RemoveQueryParams = append([]string(nil), def.RemoveQueryParams...)
		if len(site.RemoveQueryParams) == 0 {
			site.RemoveQueryParams = append([]string(nil), defaultTrackingParams...)
		}
	}
	if site.UserAgent == "" {
		site.UserAgent = def.UserAgent
		if site.UserAgent == "" {
			site.UserAgent = "pair-wise-crawler/1.0"
		}
	}
	if site.Headers == nil {
		site.Headers = make(map[string]string)
	}
	for k, v := range def.Headers {
		if _, exists := site.Headers[k]; !exists {
			site.Headers[k] = v
		}
	}
}

func firstNonZero(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

func (c *Config) Site(name string) (*Site, bool) {
	for i := range c.Sites {
		if c.Sites[i].Name == name {
			return &c.Sites[i], true
		}
	}
	return nil, false
}

func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	out := *c
	out.Sites = append([]Site(nil), c.Sites...)
	for i := range out.Sites {
		out.Sites[i].StartURLs = append([]string(nil), c.Sites[i].StartURLs...)
		out.Sites[i].TimeLayouts = append([]string(nil), c.Sites[i].TimeLayouts...)
		out.Sites[i].RemoveQueryParams = append([]string(nil), c.Sites[i].RemoveQueryParams...)
	}
	return &out
}
