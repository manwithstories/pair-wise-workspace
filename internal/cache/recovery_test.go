package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"mediatimeline/internal/mediaerr"
)

func writeSource(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStorePutGet(t *testing.T) {
	dir := t.TempDir()
	src := writeSource(t, dir, "in.bin", "hello")
	digests, err := DigestSources([]string{src})
	if err != nil {
		t.Fatal(err)
	}
	key := Key(KeyInput{Stage: "scan", Params: "{}", Sources: []string{src}})
	e := &Entry{Key: key, Stage: "scan", SourceDigests: digests,
		ResultDigest: "abc", Payload: json.RawMessage(`{"n":1}`)}
	store := &Store{Dir: filepath.Join(dir, "cache")}
	if err := store.Put(e); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ResultDigest != "abc" {
		t.Fatalf("got %+v", got)
	}
	if err := store.ValidateSources(got); err != nil {
		t.Fatal(err)
	}
}

func TestStoreMiss(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	got, err := store.Get("deadbeef")
	if err != nil || got != nil {
		t.Fatalf("miss = %v, %v", got, err)
	}
}

func TestStoreTamperedEntry(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Dir: filepath.Join(dir, "cache")}
	key := Key(KeyInput{Stage: "s", Params: "p"})
	if err := store.Put(&Entry{Key: key, Stage: "s", ResultDigest: "x",
		SourceDigests: map[string]string{}, Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	// Tamper with the cache file: flip the embedded key.
	data, _ := os.ReadFile(store.path(key))
	data[len(data)/2] ^= 0x01
	if err := os.WriteFile(store.path(key), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(key); err == nil {
		t.Fatal("tampered entry accepted")
	} else if !mediaerr.Recoverable(err) {
		t.Fatalf("tamper error not recoverable: %v", err)
	}
}

func TestStoreSourceDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	src := writeSource(t, dir, "in.bin", "original")
	digests, _ := DigestSources([]string{src})
	key := Key(KeyInput{Stage: "scan", Params: "{}", Sources: []string{src}})
	store := &Store{Dir: filepath.Join(dir, "cache")}
	if err := store.Put(&Entry{Key: key, Stage: "scan", SourceDigests: digests,
		ResultDigest: "r", Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	// Modify the source after caching: reuse must be refused.
	writeSource(t, dir, "in.bin", "modified")
	got, err := store.Get(key)
	if err != nil || got == nil {
		t.Fatalf("get = %v, %v", got, err)
	}
	if err := store.ValidateSources(got); err == nil {
		t.Fatal("modified source accepted")
	} else if !mediaerr.Recoverable(err) {
		t.Fatalf("not recoverable: %v", err)
	}
}

func TestCheckpointAtomic(t *testing.T) {
	dir := t.TempDir()
	cp := &Checkpoint{Stage: "scan", Key: "k", ResultDigest: "r", DurationMicros: 5}
	if err := WriteCheckpoint(dir, "scan", cp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(CheckpointPath(dir, "scan") + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
	got, err := ReadCheckpoint(dir, "scan")
	if err != nil || got == nil || got.Key != "k" {
		t.Fatalf("read = %+v, %v", got, err)
	}
}

func TestRecoveryFlow(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Dir: filepath.Join(dir, "cache")}
	cpDir := filepath.Join(dir, "checkpoints")
	src := writeSource(t, dir, "in.bin", "data")
	stages := []string{"scan", "clock", "compose", "render"}

	// run executes stages from the beginning, honouring checkpoints,
	// and stops after `interruptAfter` stages (0 = no interrupt).
	run := func(interruptAfter int) (recomputed []string) {
		dep := ""
		for i, st := range stages {
			key := Key(KeyInput{Stage: st, Params: "p", Sources: []string{src}, Deps: []string{dep}})
			if _, ok := ValidCheckpoint(cpDir, st, key, store); ok {
				entry, _ := store.Get(key)
				dep = entry.ResultDigest
				continue
			}
			recomputed = append(recomputed, st)
			digests, _ := DigestSources([]string{src})
			payload := []byte(`{"stage":"` + st + `"}`)
			dep = ResultDigest(payload)
			if err := store.Put(&Entry{Key: key, Stage: st, SourceDigests: digests,
				ResultDigest: dep, Payload: payload}); err != nil {
				t.Fatal(err)
			}
			if err := WriteCheckpoint(cpDir, st, &Checkpoint{
				Stage: st, Key: key, ResultDigest: dep, DurationMicros: 1}); err != nil {
				t.Fatal(err)
			}
			if interruptAfter > 0 && i+1 >= interruptAfter {
				return recomputed
			}
		}
		return recomputed
	}

	// First run is interrupted after two stages.
	if got := run(2); len(got) != 2 || got[0] != "scan" || got[1] != "clock" {
		t.Fatalf("first run recomputed %v", got)
	}
	// Resume recomputes only the remaining stages.
	if got := run(0); len(got) != 2 || got[0] != "compose" || got[1] != "render" {
		t.Fatalf("resume recomputed %v", got)
	}
	// A clean repeat run recomputes nothing.
	if got := run(0); len(got) != 0 {
		t.Fatalf("repeat recomputed %v", got)
	}
	// Tamper with the compose checkpoint: compose is recomputed. Its
	// result digest is identical, so render's checkpoint stays valid
	// and render is not recomputed.
	cp, _ := ReadCheckpoint(cpDir, "compose")
	cp.ResultDigest = "forged"
	if err := WriteCheckpoint(cpDir, "compose", cp); err != nil {
		t.Fatal(err)
	}
	if got := run(0); len(got) != 1 || got[0] != "compose" {
		t.Fatalf("after tamper recomputed %v", got)
	}
}
