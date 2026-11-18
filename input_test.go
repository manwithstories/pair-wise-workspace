package main

import (
	"errors"
	"strings"
	"testing"
)

func writeTempJSONL(t *testing.T, lines ...string) string {
	t.Helper()
	p := t.TempDir() + "/in.jsonl"
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := osWriteFile(p, []byte(content)); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestScanValidRecords(t *testing.T) {
	p := writeTempJSONL(t,
		`{"track":"video","epoch":0,"seq":0,"pts":0,"num":1,"den":1000000,"bits":32,"sync_id":"s1"}`,
		`{"track":"audio","epoch":0,"seq":0,"pts":100,"num":1,"den":768000,"bits":33}`,
	)
	var got []*Record
	err := ScanRecords(mustOpen(t, p), func(r *Record) error {
		got = append(got, r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 records, got %d", len(got))
	}
	if !got[0].HasSync || got[0].SyncID != "s1" {
		t.Fatalf("sync parse failed: %+v", got[0])
	}
	if got[1].HasSync {
		t.Fatal("missing sync_id must decode as HasSync=false")
	}
	if got[1].Bits != 33 {
		t.Fatalf("bits parse: %d", got[1].Bits)
	}
}

func TestScanValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{"bad track", `{"track":"sub","epoch":0,"seq":0,"pts":0,"num":1,"den":1000,"bits":32}`, "track"},
		{"bad bits", `{"track":"video","epoch":0,"seq":0,"pts":0,"num":1,"den":1000,"bits":16}`, "bits"},
		{"zero den", `{"track":"video","epoch":0,"seq":0,"pts":0,"num":1,"den":0,"bits":32}`, "timebase"},
		{"neg num", `{"track":"video","epoch":0,"seq":0,"pts":0,"num":-1,"den":1000,"bits":32}`, "timebase"},
		{"neg pts", `{"track":"video","epoch":0,"seq":0,"pts":-5,"num":1,"den":1000,"bits":32}`, "pts"},
		{"empty sync", `{"track":"video","epoch":0,"seq":0,"pts":0,"num":1,"den":1000,"bits":32,"sync_id":""}`, "sync_id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := writeTempJSONL(t, c.line)
			err := ScanRecords(mustOpen(t, p), func(*Record) error { return nil })
			var le *LineError
			if !errors.As(err, &le) {
				t.Fatalf("want *LineError, got %T %v", err, err)
			}
			if le.Line != 1 {
				t.Fatalf("want line 1, got %d", le.Line)
			}
			if !strings.Contains(le.Msg, c.want) {
				t.Fatalf("msg %q does not mention %q", le.Msg, c.want)
			}
		})
	}
}

func TestScanLineNumbersAndBlankLines(t *testing.T) {
	// 第一行空白跳过，第二行非法 JSON 必须仍报 line 2。
	p := writeTempJSONL(t,
		"   ",
		`{not json`,
	)
	err := ScanRecords(mustOpen(t, p), func(*Record) error { return nil })
	var le *LineError
	if !errors.As(err, &le) || le.Line != 2 {
		t.Fatalf("want line 2 error, got %v", err)
	}
}

func TestScanUnknownFieldRejected(t *testing.T) {
	p := writeTempJSONL(t,
		`{"track":"video","epoch":0,"seq":0,"pts":0,"num":1,"den":1000,"bits":32,"extra":1}`,
	)
	err := ScanRecords(mustOpen(t, p), func(*Record) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("unknown field must be rejected with line: %v", err)
	}
}
