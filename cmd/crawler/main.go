package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"crawler/internal/config"
	"crawler/internal/demo"
	"crawler/internal/pipeline"
	"crawler/internal/report"
	"crawler/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "run":
		err = runCommand(ctx, os.Args[2:])
	case "replay":
		err = replayCommand(ctx, os.Args[2:])
	case "stats":
		err = statsCommand(ctx, os.Args[2:])
	case "demo":
		err = demoCommand(ctx, os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  crawler run    --mode full|incr --config config/sites.yaml [--db data/crawl.db] [--report report.json]
  crawler replay --class timeout|refused|http_4xx|http_5xx|rate_limited|parse --config ... [--db ...]
  crawler stats  [--db data/crawl.db]
  crawler demo   [--pages 600] [--sites 3] [--db data/crawl.db] [--report report.json]`)
}

func runCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := fs.String("config", "config/sites.yaml", "site configuration file")
	dbPath := fs.String("db", "data/crawl.db", "SQLite database path")
	modeRaw := fs.String("mode", "incr", "full or incr")
	reportPath := fs.String("report", "", "write JSON run report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	mode := pipeline.Mode(*modeRaw)
	if mode != pipeline.ModeFull && mode != pipeline.ModeIncremental {
		return fmt.Errorf("mode must be full or incr")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	stats, err := pipeline.New(cfg, st).Run(ctx, mode)
	if err != nil {
		return err
	}
	r := report.Report{}
	if mode == pipeline.ModeFull {
		r.Full = &stats
		fmt.Println(stats.FullLine())
	} else {
		r.Incremental = &stats
		fmt.Println(stats.IncrementalLine())
	}
	return r.WriteJSON(*reportPath)
}

func replayCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	class := fs.String("class", "parse", "failure class to replay")
	cfgPath := fs.String("config", "config/sites.yaml", "site configuration file")
	dbPath := fs.String("db", "data/crawl.db", "SQLite database path")
	reportPath := fs.String("report", "", "write JSON replay report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch classValue := *class; classValue {
	case "timeout", "refused", "http_4xx", "http_5xx", "rate_limited", "parse", "network":
	default:
		return fmt.Errorf("unsupported failure class %q", classValue)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	stats, err := pipeline.New(cfg, st).Replay(ctx, *class)
	if err != nil {
		return err
	}
	fmt.Printf("mode=replay class=%s fetched=%d new=%d revised=%d dedup=%d failed=%d rows=%d\n",
		*class, stats.Fetched, stats.New, stats.Revised, stats.Dedup, stats.Failed, stats.Rows)
	r := report.Report{Incremental: &stats}
	return r.WriteJSON(*reportPath)
}

func statsCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	dbPath := fs.String("db", "data/crawl.db", "SQLite database path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	rows, err := st.CountActiveAnnouncements(ctx)
	if err != nil {
		return err
	}
	storedRows, err := st.CountAnnouncements(ctx)
	if err != nil {
		return err
	}
	versions, err := st.CountVersions(ctx)
	if err != nil {
		return err
	}
	aliases, err := st.CountAliases(ctx)
	if err != nil {
		return err
	}
	wm, err := st.ReadWatermark(ctx, "incr")
	if err != nil && !isNoWatermark(err) {
		return err
	}
	last, err := st.LastRun(ctx)
	if err == nil {
		stats := report.RunStats{
			RunID: last.ID, Mode: last.Mode, Fetched: last.Fetched, RawURL: last.RawURL,
			UniqURL: last.UniqURL, Dedup: last.Dedup, New: last.New, Revised: last.Revised,
			Failed: last.Failed, Failures: last.Failures, Watermark: last.Watermark, Rows: last.Rows,
			StartedAt: last.StartedAt, FinishedAt: last.FinishedAt,
		}
		fmt.Println(stats.FullLine())
	}
	failures, err := st.FailureCounts(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("rows=%d stored_rows=%d versions=%d aliases=%d watermark=%s\n", rows, storedRows, versions, aliases, wm.String())
	for _, f := range failures {
		fmt.Printf("failed_%s=%d\n", f.Class, f.Count)
	}
	return nil
}

func demoCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	pages := fs.Int("pages", 600, "number of local page requests")
	sites := fs.Int("sites", 3, "number of local sites")
	dbPath := fs.String("db", "data/crawl.db", "SQLite database path")
	reportPath := fs.String("report", "report.json", "write JSON demonstration report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	result, err := demo.Run(ctx, demo.Options{
		Pages: *pages, Sites: *sites, DBPath: *dbPath, ReportPath: *reportPath,
	})
	if err != nil {
		return err
	}
	for _, line := range result.Lines() {
		fmt.Println(line)
	}
	if *reportPath != "" {
		if _, err := os.Stat(*reportPath); err != nil {
			return fmt.Errorf("report was not written: %w", err)
		}
	}
	return nil
}

func isNoWatermark(err error) bool {
	return err == store.ErrNoWatermark
}
