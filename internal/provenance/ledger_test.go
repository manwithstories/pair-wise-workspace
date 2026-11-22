package provenance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"example.com/medialab/archive/internal/model"
)

func sha(ch byte) string { return strings.Repeat(string(ch), 64) }

func entry(rep model.RepresentationID, index, ver int, digest string) Entry {
	return Entry{
		Rep: rep, SegmentIndex: index, Version: ver, StartUS: int64(index) * 1e7,
		EndUS: int64(index+1) * 1e7, OutputSHA256: digest,
		SourceStream: "s", SourceSeqMin: 1, SourceSeqMax: 9, SourcePacketCount: 9,
		TimelineVersion: ver,
	}
}

func TestRecordChainAndIdempotency(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	ok, err := w.Record(entry("64k", 0, 1, sha('a')))
	if err != nil || !ok {
		t.Fatal(err)
	}
	d1 := w.Digest()
	ok, err = w.Record(entry("64k", 0, 1, sha('a')))
	if err != nil || ok || w.Digest() != d1 || w.Count() != 1 {
		t.Fatalf("duplicate changed ledger: %v/%v", ok, err)
	}
	if _, err := w.Record(entry("64k", 0, 1, sha('b'))); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict err = %v", err)
	}
	replay := entry("64k", 0, 2, sha('c'))
	replay.ReplacesVersion = 1
	replay.ReplacesSHA256 = sha('a')
	ok, err = w.Record(replay)
	if err != nil || !ok || w.Digest() == d1 {
		t.Fatalf("replay record = %v/%v", ok, err)
	}
	// Lines are valid JSON and carry sequence numbers.
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d", len(lines))
	}
	for i, line := range lines {
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		if e.Seq != i {
			t.Fatalf("seq %d = %d", i, e.Seq)
		}
		if e.OutputSHA256 == "" {
			t.Fatal("missing output hash in ledger")
		}
	}
}

func TestRecordValidation(t *testing.T) {
	w := NewWriter(nil)
	for i, e := range []Entry{
		{},
		{Rep: "64k", SegmentIndex: -1, Version: 1, OutputSHA256: sha('a')},
		{Rep: "64k", SegmentIndex: 0, Version: 0, OutputSHA256: sha('a')},
		{Rep: "64k", SegmentIndex: 0, Version: 1, OutputSHA256: "nope"},
	} {
		if _, err := w.Record(e); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}

func TestRestoreDigest(t *testing.T) {
	w := NewWriter(nil)
	w.Record(entry("64k", 0, 1, sha('a')))
	d := w.Digest()

	r := NewWriter(nil)
	if err := r.Restore(d, 1); err != nil {
		t.Fatal(err)
	}
	r.Remember("64k", 0, 1, sha('a'))
	// Same entry after restore is silently ignored.
	if ok, err := r.Record(entry("64k", 0, 1, sha('a'))); err != nil || ok {
		t.Fatalf("post-restore duplicate = %v/%v", ok, err)
	}
	if r.Digest() != d || r.Count() != 1 {
		t.Fatal("restore changed digest")
	}
	if err := r.Restore("bad", 0); err == nil {
		t.Fatal("invalid digest accepted")
	}
}

func TestConcurrentRecord(t *testing.T) {
	w := NewWriter(nil)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := w.Record(entry("64k", i, 1, fmt.Sprintf("%064d", i))); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if w.Count() != 16 {
		t.Fatalf("count = %d", w.Count())
	}
}
