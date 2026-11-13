package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func testRecord(key, url, hash, body string, published time.Time) Record {
	return Record{
		RawURL: url, CanonicalURL: url, Site: "s1", Title: "公告 " + key,
		TitleNorm: "公告 " + key, ProjectNo: key, BusinessKey: key,
		PublishTime: published, Body: body, ContentHash: hash,
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "crawl.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestApplyBatchRevisionChainAndIdempotence(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	batch := []Record{
		testRecord("P-1", "http://x/1", "v1", "版本一", now),
		testRecord("P-1", "http://y/1", "v1", "版本一", now),
		testRecord("P-1", "http://x/1?rev=2", "v2", "版本二", now.Add(time.Minute)),
	}
	res, err := st.ApplyBatch(ctx, batch, ApplyOptions{Mode: "full", AdvanceWatermark: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.New != 1 || res.Revised != 1 || res.Duplicate != 1 || res.Rows != 1 {
		t.Fatalf("unexpected first apply: %+v", res)
	}
	versions, _ := st.CountVersions(ctx)
	aliases, _ := st.CountAliases(ctx)
	if versions != 2 || aliases != 3 {
		t.Fatalf("versions=%d aliases=%d", versions, aliases)
	}
	res, err = st.ApplyBatch(ctx, batch, ApplyOptions{Mode: "full", AdvanceWatermark: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.New != 0 || res.Revised != 0 || res.Rows != 1 {
		t.Fatalf("replay should be idempotent: %+v", res)
	}
	versions, _ = st.CountVersions(ctx)
	if versions != 2 {
		t.Fatalf("idempotent replay changed version chain: %d", versions)
	}
}

func TestApplyBatchRollsBackAndWatermark(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	good := testRecord("P-1", "http://x/1", "v1", "正文", now)
	bad := testRecord("P-2", "http://x/2", "v1", "正文", now)
	bad.BusinessKey = ""
	if _, err := st.ApplyBatch(ctx, []Record{good, bad}, ApplyOptions{Mode: "full", AdvanceWatermark: true}); err == nil {
		t.Fatal("expected invalid batch to fail")
	}
	rows, _ := st.CountAnnouncements(ctx)
	if rows != 0 {
		t.Fatalf("batch should have rolled back, rows=%d", rows)
	}
	if _, err := st.ReadWatermark(ctx, "incr"); err != ErrNoWatermark {
		t.Fatalf("watermark should not advance on rollback: %v", err)
	}
	if _, err := st.ApplyBatch(ctx, []Record{good}, ApplyOptions{Mode: "full", AdvanceWatermark: true}); err != nil {
		t.Fatal(err)
	}
	wm, err := st.ReadWatermark(ctx, "incr")
	if err != nil {
		t.Fatal(err)
	}
	if wm.String() != "2026-11-12T09:30:00Z|P-1" {
		t.Fatalf("unexpected watermark %q", wm.String())
	}
}
