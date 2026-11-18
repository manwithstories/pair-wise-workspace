package main

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func sampleRows(n int) []OutputRow {
	rows := make([]OutputRow, n)
	for i := 0; i < n; i++ {
		track := "video"
		if i%2 == 1 {
			track = "audio"
		}
		rows[i] = OutputRow{
			Track: track, Segment: i % 3, Epoch: int64(i % 3),
			Seq: int64(i), OriginalPTS: int64(1000 + i),
			CorrectedPTS: int64(2_000_000_000 + i), // 含负值边界另测
			HasSync:      i%4 == 0, SyncID: "sync-x",
		}
	}
	return rows
}

func TestJSONCSVConsistent(t *testing.T) {
	dir := t.TempDir()
	jp := filepath.Join(dir, "out.json")
	cp := filepath.Join(dir, "out.csv")
	rows := sampleRows(50)

	jw, err := NewJSONWriter(jp)
	if err != nil {
		t.Fatal(err)
	}
	cw, err := NewCSVWriter(cp)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if err := jw.Write(r); err != nil {
			t.Fatal(err)
		}
		if err := cw.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := jw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}

	// JSON 必须是合法数组且行数一致。
	raw, err := os.ReadFile(jp)
	if err != nil {
		t.Fatal(err)
	}
	var got []jsonRow
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json output not valid array: %v", err)
	}
	if len(got) != len(rows) {
		t.Fatalf("json rows %d want %d", len(got), len(rows))
	}

	// CSV 表头 + 数据行，corrected_pts 与 JSON 逐项一致。
	f, _ := os.Open(cp)
	defer f.Close()
	rd := csv.NewReader(f)
	all, err := rd.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(rows)+1 {
		t.Fatalf("csv lines %d want %d", len(all), len(rows)+1)
	}
	if all[0][0] != "track" || all[0][6] != "sync_id" {
		t.Fatalf("csv header wrong: %v", all[0])
	}
	for i, r := range rows {
		line := all[i+1]
		if line[5] != strconv.FormatInt(r.CorrectedPTS, 10) {
			t.Fatalf("row %d csv corrected %s != %d", i, line[5], r.CorrectedPTS)
		}
		if line[5] != strconv.FormatInt(int64(got[i].CorrectedPTS), 10) {
			t.Fatalf("row %d csv/json mismatch", i)
		}
		if line[0] != r.Track || line[1] != strconv.Itoa(r.Segment) {
			t.Fatalf("row %d identity fields wrong: %v", i, line)
		}
	}
}

func TestNegativeCorrectedPreserved(t *testing.T) {
	// 负校正时间必须原样保留，不允许被钳到零。
	dir := t.TempDir()
	cp := filepath.Join(dir, "n.csv")
	cw, _ := NewCSVWriter(cp)
	row := OutputRow{Track: "audio", Segment: 0, Epoch: 0, Seq: 1,
		OriginalPTS: 10, CorrectedPTS: -12345, HasSync: false}
	if err := cw.Write(row); err != nil {
		t.Fatal(err)
	}
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(cp)
	if !strings.Contains(string(data), "-12345") {
		t.Fatalf("negative corrected pts not preserved: %s", data)
	}
}

func TestAbortLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	jp := filepath.Join(dir, "a.json")
	jw, err := NewJSONWriter(jp)
	if err != nil {
		t.Fatal(err)
	}
	if err := jw.Write(sampleRows(1)[0]); err != nil {
		t.Fatal(err)
	}
	jw.Abort()
	if _, err := os.Stat(jp); !os.IsNotExist(err) {
		t.Fatalf("abort must remove partial output, stat err=%v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".avsync-") {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
}

func TestSyncIDOmitEmpty(t *testing.T) {
	dir := t.TempDir()
	jp := filepath.Join(dir, "o.json")
	jw, _ := NewJSONWriter(jp)
	_ = jw.Write(OutputRow{Track: "video", CorrectedPTS: 1})
	_ = jw.Close()
	data, _ := os.ReadFile(jp)
	if strings.Contains(string(data), "sync_id") {
		t.Fatalf("sync_id must be omitted when absent: %s", data)
	}
}
