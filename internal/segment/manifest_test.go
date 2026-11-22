package segment

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"example.com/medialab/archive/internal/model"
)

func sha(ch byte) string { return strings.Repeat(string(ch), 64) }

func entry(rep model.RepresentationID, index, ver int, digest string) Entry {
	return Entry{
		Rep: rep, Index: index, Version: ver, StartUS: int64(index) * 1e7, EndUS: int64(index+1) * 1e7,
		OutputSHA256: digest,
	}
}

func TestCommitIdempotentAndChained(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	ok, err := w.Commit(entry("64k", 0, 1, sha('a')))
	if err != nil || !ok {
		t.Fatalf("first commit = %v/%v", ok, err)
	}
	chain1 := w.ChainHex()
	// Same content again: ignored, digest unchanged.
	ok, err = w.Commit(entry("64k", 0, 1, sha('a')))
	if err != nil || ok || w.ChainHex() != chain1 {
		t.Fatalf("duplicate commit changed state: %v/%v", ok, err)
	}
	// Conflicting payload for the same version is rejected.
	if _, err := w.Commit(entry("64k", 0, 1, sha('b'))); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("conflict err = %v", err)
	}
	// A new version must follow and must reference the replaced version.
	replay := entry("64k", 0, 2, sha('c'))
	replay.ReplacesVersion = 1
	if _, err := w.Commit(replay); err != nil {
		t.Fatal(err)
	}
	if w.Count() != 2 {
		t.Fatalf("count = %d", w.Count())
	}
	if w.ChainHex() == chain1 {
		t.Fatal("chain digest did not advance")
	}
}

func TestCommitVersionRules(t *testing.T) {
	w := NewWriter(nil)
	e2 := entry("64k", 0, 2, sha('a'))
	e2.ReplacesVersion = 1
	if _, err := w.Commit(e2); !errors.Is(err, ErrVersionGap) {
		t.Fatalf("first version must be 1: %v", err)
	}
	if _, err := w.Commit(entry("64k", 0, 1, sha('a'))); err != nil {
		t.Fatal(err)
	}
	skip := entry("64k", 0, 3, sha('b'))
	skip.ReplacesVersion = 2
	if _, err := w.Commit(skip); !errors.Is(err, ErrVersionGap) {
		t.Fatalf("skipping a version accepted: %v", err)
	}
	e := entry("64k", 0, 2, sha('c'))
	e.ReplacesVersion = 5
	if _, err := w.Commit(e); err == nil {
		t.Fatal("wrong replaces_version accepted")
	}
}

func TestValidateRejectsBadEntries(t *testing.T) {
	bad := []Entry{
		{Rep: "", Index: 0, Version: 1, EndUS: 1, OutputSHA256: sha('a')},
		{Rep: "x", Index: -1, Version: 1, EndUS: 1, OutputSHA256: sha('a')},
		{Rep: "x", Index: 0, Version: 0, EndUS: 1, OutputSHA256: sha('a')},
		{Rep: "x", Index: 0, Version: 1, StartUS: 5, EndUS: 5, OutputSHA256: sha('a')},
		{Rep: "x", Index: 0, Version: 1, EndUS: 1, OutputSHA256: "short"},
		{Rep: "x", Index: 0, Version: 1, EndUS: 1, OutputSHA256: strings.Repeat("z", 64)},
	}
	for i, e := range bad {
		if _, err := NewWriter(nil).Commit(e); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}

func TestRestoreAndDuplicateAfterResume(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.Commit(entry("64k", 0, 1, sha('a')))
	chain, count := w.ChainHex(), w.Count()

	var out bytes.Buffer
	r := NewWriter(&out)
	if err := r.Restore(chain, count); err != nil {
		t.Fatal(err)
	}
	r.SetCursor("64k", 0, 1, sha('a'))
	ok, err := r.Commit(entry("64k", 0, 1, sha('a')))
	if err != nil || ok {
		t.Fatalf("resumed duplicate = %v/%v", ok, err)
	}
	if r.ChainHex() != chain || r.Count() != count {
		t.Fatal("resume changed the digest")
	}
	// A replay after resume must still chain correctly.
	replay := entry("64k", 0, 2, sha('d'))
	replay.ReplacesVersion = 1
	if _, err := r.Commit(replay); err != nil {
		t.Fatal(err)
	}
	if c := r.Count(); c != count+1 {
		t.Fatalf("count after resume = %d", c)
	}
	if err := r.Restore("zz", 0); err == nil {
		t.Fatal("bad digest accepted")
	}
}

func TestConcurrentCommitSerialises(t *testing.T) {
	w := NewWriter(nil)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := entry("64k", i, 1, sha('a'))
			if _, err := w.Commit(e); err != nil {
				t.Errorf("commit %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if w.Count() != 32 {
		t.Fatalf("count = %d", w.Count())
	}
}

func TestManifestLinesAreJSON(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.Commit(entry("64k", 0, 1, sha('a')))
	sc := bufio.NewScanner(&buf)
	n := 0
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line %d: %v", n, err)
		}
		n++
	}
	if n != 1 {
		t.Fatalf("lines = %d", n)
	}
}
