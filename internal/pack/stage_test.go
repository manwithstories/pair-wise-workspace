package pack

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestPCMStageAppendIdempotentAfterCheckpoint(t *testing.T) {
	stage := &PCMStage{Path: filepath.Join(t.TempDir(), "audio.pcm"), Channels: 2}
	first := []byte{0, 0, 1, 0, 2, 0, 3, 0} // two stereo frames
	second := []byte{4, 0, 5, 0, 6, 0, 7, 0}

	if err := stage.AppendAt(0, first); err != nil {
		t.Fatal(err)
	}
	// Checkpoint says two frames committed. A crash may have appended an
	// uncommitted duplicate/tail; resume must truncate and rewrite second.
	dirty := append(append([]byte{}, second...), 0xde, 0xad, 0xbe, 0xef)
	if err := stage.AppendAt(2, dirty); err != nil {
		t.Fatal(err)
	}
	// Correct resume call writes only the real next chunk at offset two.
	if err := stage.AppendAt(2, second); err != nil {
		t.Fatal(err)
	}
	got, err := stage.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte{}, first...), second...)
	if !bytes.Equal(got, want) {
		t.Fatalf("staged PCM=%v, want %v (no duplicate/tail)", got, want)
	}
}

func TestPCMStageRejectsCheckpointBeyondFile(t *testing.T) {
	stage := &PCMStage{Path: filepath.Join(t.TempDir(), "audio.pcm"), Channels: 2}
	if err := stage.AppendAt(0, []byte{1, 0, 2, 0}); err != nil {
		t.Fatal(err)
	}
	if err := stage.AppendAt(10, []byte{3, 0, 4, 0}); err == nil {
		t.Fatal("checkpoint beyond durable PCM must be rejected")
	}
}
