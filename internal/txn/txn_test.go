package txn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediarig/internal/media"
)

func TestWriteAtomicIsSameDirectoryAndDurableContent(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "stats.env")
	if err := WriteAtomic(final, []byte("commit=true\n")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(final)
	if err != nil || string(b) != "commit=true\n" {
		t.Fatalf("read file content=%q err=%v", b, err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestWALTailIncompleteRecordIsTruncated(t *testing.T) {
	p, err := Layout(filepath.Join(t.TempDir(), "rig"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	w, err := OpenWAL(p.WAL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.AppendCheckpoint(media.StageAudio, "in", "par", nil, "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.AppendCheckpoint(media.StageColor, "in", "par", nil, "two"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(p.WAL)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.WAL, append(good, []byte(`{"seq":3,"stage":"mp4","checksum":"bad`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := ReadAndRepair(p.WAL)
	if err != nil {
		t.Fatalf("torn tail should repair: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("records=%d want2", len(recs))
	}
	repaired, err := os.ReadFile(p.WAL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(repaired), "\n") {
		t.Fatal("repaired WAL should end at previous record boundary")
	}
}

func TestWALMiddleChecksumFailureRejected(t *testing.T) {
	p, _ := Layout(filepath.Join(t.TempDir(), "rig"))
	_ = p.Ensure()
	w, err := OpenWAL(p.WAL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.AppendCheckpoint(media.StageAudio, "", "", nil, "one")
	_, _ = w.AppendCheckpoint(media.StageColor, "", "", nil, "two")
	_ = w.Close()
	data, err := os.ReadFile(p.WAL)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	lines[0] = strings.Replace(lines[0], `"stage":"audio"`, `"stage":"AUDIO"`, 1)
	corrupt := []byte(strings.Join(lines, "\n") + "\n")
	if err := os.WriteFile(p.WAL, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ReadAndRepair(p.WAL)
	var re *media.Error
	if err == nil || !errorAsRig(err, &re) || re.Stage != media.StageRecover || re.Code != 2 {
		t.Fatalf("expected recovery corruption, got %v", err)
	}
}

func errorAsRig(err error, target **media.Error) bool {
	for err != nil {
		if re, ok := err.(*media.Error); ok {
			*target = re
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestRecoveryVerifiesParamsInputArtifactsAndCleansTemps(t *testing.T) {
	p, _ := Layout(filepath.Join(t.TempDir(), "rig"))
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.InputFile, []byte("fixture-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	params := Params{Root: p.Root, Rate: 48000, Layout: "stereo", LUFS: -23, TargetKbps: 1536}
	if err := WriteJSONAtomic(p.Params, params); err != nil {
		t.Fatal(err)
	}
	pd, err := params.Digest()
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := SHA256File(p.InputFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(p.AudioPCM, []byte("pcm")); err != nil {
		t.Fatal(err)
	}
	d, n, err := SHA256File(p.AudioPCM)
	if err != nil {
		t.Fatal(err)
	}
	w, err := OpenWAL(p.WAL)
	if err != nil {
		t.Fatal(err)
	}
	art := Artifact{Name: "audio.s16le", Path: p.AudioPCM, SHA256: d, Size: n}
	if _, err := w.AppendCheckpoint(media.StageAudio, id, pd, []Artifact{art}, ""); err != nil {
		t.Fatal(err)
	}
	w.Close()
	tmp := filepath.Join(p.OutputDir, ".tmp-master.mp4-x")
	if err := os.WriteFile(tmp, []byte("temp"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, err := LoadAndVerify(p.Root)
	if err != nil {
		t.Fatalf("recovery verify: %v", err)
	}
	if !rec.StageDone(media.StageAudio) {
		t.Fatal("audio checkpoint not found")
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("uncommitted temp was not removed")
	}

	if err := os.WriteFile(p.AudioPCM, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAndVerify(p.Root); err == nil {
		t.Fatal("tampered committed artifact accepted")
	}
}

func TestWithinRootAndSymlinkOutput(t *testing.T) {
	root := t.TempDir()
	p, _ := Layout(root)
	if !WithinRoot(p.Root, p.MP4) {
		t.Fatal("MP4 should be within root")
	}
	if WithinRoot(p.Root, filepath.Join(p.Root, "..", "outside")) {
		t.Fatal("parent traversal accepted")
	}
	link := filepath.Join(root, "link.mp4")
	if err := os.Symlink(filepath.Join(root, "target.mp4"), link); err != nil {
		t.Skip(err)
	}
	if err := RejectSymlinkOutput(link); err == nil {
		t.Fatal("symlink output accepted")
	}
}
