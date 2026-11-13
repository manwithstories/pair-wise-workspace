package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLoadRepositoryConfig(t *testing.T) {
	path := filepath.Join("..", "..", "config", "sites.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sites) != 3 {
		t.Fatalf("sites=%d", len(cfg.Sites))
	}
	for _, site := range cfg.Sites {
		if site.Timeout.Std() == 0 || site.MaxPages == 0 || site.MaxBodyRunes == 0 {
			t.Fatalf("defaults not applied: %+v", site)
		}
		if site.Selectors.Item == "" || site.Selectors.Title == "" {
			t.Fatalf("missing selectors: %+v", site.Selectors)
		}
	}
	if cfg.Sites[0].Timeout.Std() != 10*time.Second {
		t.Fatalf("timeout=%s", cfg.Sites[0].Timeout.Std())
	}
}

func TestNormalizeRejectsBadConfig(t *testing.T) {
	cfg := Config{Sites: []Site{{Name: "x", StartURLs: []string{"ftp://example.com"}}}}
	if err := cfg.Normalize(); err == nil {
		t.Fatal("expected invalid scheme to be rejected")
	}
	cfg = Config{Sites: []Site{{Name: "x", StartURLs: []string{"http://example.com"}}}}
	if err := cfg.Normalize(); err == nil {
		t.Fatal("expected missing selector to be rejected")
	}
}
