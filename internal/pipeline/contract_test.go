package pipeline

import (
	"testing"

	"mediasim/internal/render"
)

func TestBadArgumentsRejected(t *testing.T) {
	// Apply defaults first, then break exactly one field; Defaults() would
	// otherwise repair a zero window or packet cap.
	base := func() Config {
		c := Config{Fixture: "normal", OutDir: t.TempDir()}
		c.Defaults()
		return c
	}
	cases := map[string]func(*Config){
		"negative loss":   func(c *Config) { c.LossBurst = -1 },
		"negative jitter": func(c *Config) { c.JitterMs = -1 },
		"zero window":     func(c *Config) { c.Window = -1 },
		"huge window":     func(c *Config) { c.Window = 99999 },
		"bad fixture":     func(c *Config) { c.Fixture = "nonexistent" },
	}
	for name, mutate := range cases {
		c := base()
		mutate(&c)
		if _, err := Run(c); err == nil {
			t.Fatalf("%s: accepted bad config %+v", name, c)
		}
	}
}

func TestCommitOnceAcrossRecovery(t *testing.T) {
	// Rendering the same frame index twice must be refused; this is what
	// keeps a recovery pass from changing an already committed frame.
	w, err := render.NewWriter(t.TempDir(), render.DefaultParams(17))
	if err != nil {
		t.Fatal(err)
	}
	f := render.Frame{Index: 0, Ticks: 0}
	if _, err := w.Write(f); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(f); err == nil {
		t.Fatal("duplicate commit accepted")
	}
	if w.Count() != 1 {
		t.Fatalf("count=%d", w.Count())
	}
}

func TestPercentile(t *testing.T) {
	vals := []float64{5, 1, 3, 2, 4}
	if got := Percentile(vals, 0.95); got != 5 {
		t.Fatalf("p95=%v", got)
	}
	if got := Percentile(vals, 1); got != 5 {
		t.Fatalf("max=%v", got)
	}
	if got := Percentile(nil, 0.5); got != 0 {
		t.Fatalf("empty=%v", got)
	}
}
