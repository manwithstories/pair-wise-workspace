package demo

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"crawler/internal/report"
)

func TestAcceptanceDemo(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crawl.db")
	reportPath := filepath.Join(dir, "report.json")
	started := time.Now()
	r, err := Run(context.Background(), Options{
		Pages: 600, Sites: 3, DBPath: dbPath, ReportPath: reportPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 60*time.Second {
		t.Fatalf("demo took %s", elapsed)
	}
	lines := r.Lines()
	want := []string{
		"mode=full fetched=600 raw_url=612 uniq_url=591 dedup=21(3.4%) new=573 revised=17 failed=29(timeout=9,refused=1,http_4xx=11,http_5xx=2,parse=6) watermark=2026-11-12T09:30:00Z|P-0591",
		"mode=incr fetched=140 new=22 revised=3 dedup=115",
		"rerun=new:0 revised:0 rows=593 idempotent=true",
	}
	if len(lines) != len(want) {
		t.Fatalf("lines=%q", lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d:\n got %q\nwant %q", i+1, lines[i], want[i])
		}
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded report.Report
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Rerun == nil || decoded.Rerun.Rows != 593 || !decoded.Rerun.Idempotent ||
		decoded.Full.New != 573 || decoded.Incremental.New != 22 {
		t.Fatalf("report JSON does not match lines: %+v", decoded)
	}
	size, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if size.Size() > 50<<20 {
		t.Fatalf("database too large: %d", size.Size())
	}
}
