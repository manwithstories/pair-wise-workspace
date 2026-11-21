package journal

import (
	"os"
	"path/filepath"
	"testing"

	"mediamerge/internal/model"
)

func TestAppendAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.log")
	l, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{
		{Stage: 0, Kind: KindBegin, InputSHA256: "aa", InputSize: 100, OutputLen: 16},
		{Stage: 0, Kind: KindCommit, InputSHA256: "aa", InputSize: 100, OutputLen: 16, PrefixSHA: "bb"},
		{Stage: 1, Kind: KindBegin, InputSHA256: "aa", InputSize: 100, OutputLen: 16},
		{Stage: 1, Kind: KindCommit, InputSHA256: "aa", InputSize: 100, OutputLen: 2500, PrefixSHA: "cc"},
	}
	for _, r := range want {
		if err := l.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("records=%d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d = %+v want %+v", i, got[i], want[i])
		}
	}
}

func TestTornTailIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.log")
	l, _ := Create(path)
	_ = l.Append(Record{Stage: 0, Kind: KindBegin, InputSHA256: "aa", OutputLen: 16})
	_ = l.Append(Record{Stage: 0, Kind: KindCommit, InputSHA256: "aa", OutputLen: 16, PrefixSHA: "bb"})
	_ = l.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(`{"stage":1,"kind":"beg`)
	_ = f.Close()
	got, err := Load(path)
	if err != nil {
		t.Fatalf("torn tail rejected: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("records=%d", len(got))
	}
}

func TestMalformedCompleteLineFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.log")
	if err := os.WriteFile(path, []byte("{\"stage\":0}\nnot-json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected integrity error")
	}
	pe, ok := err.(*model.Error)
	if !ok || pe.Code != model.CodeIntegrity {
		t.Fatalf("err=%v", err)
	}
	if pe.Offset != int64(len("{\"stage\":0}\n")) {
		t.Fatalf("offset=%d", pe.Offset)
	}
}
