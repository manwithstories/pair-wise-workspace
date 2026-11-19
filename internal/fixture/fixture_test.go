package fixture

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"avpipe/internal/audio"
)

func TestSignalProducesParseableBlocks(t *testing.T) {
	layout, _ := audio.ParseLayout("stereo")
	blocks, data, f, err := Signal(SignalConfig{
		SampleRate: 48000, Layout: layout, Frames: 4800, BlockSize: 1024, Seed: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 5 || blocks[4].Frames != 4800-4*1024 {
		t.Fatalf("framing %d blocks, last=%d", len(blocks), blocks[len(blocks)-1].Frames)
	}
	info, err := audio.Parse(data, "stereo")
	if err != nil {
		t.Fatal(err)
	}
	if info.Format.Channels != 2 || info.BlockFrames != 4800 || f.BitsPerSample != 16 {
		t.Fatalf("info=%+v", info)
	}
}

func TestSignalDeterministic(t *testing.T) {
	layout, _ := audio.ParseLayout("mono")
	_, a, _, _ := Signal(SignalConfig{Layout: layout, Frames: 1000, NoiseAmp: 0.01, Seed: 7})
	_, b, _, _ := Signal(SignalConfig{Layout: layout, Frames: 1000, NoiseAmp: 0.01, Seed: 7})
	if len(a) != len(b) {
		t.Fatal("length differs")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("byte %d differs", i)
		}
	}
	_, c, _, _ := Signal(SignalConfig{Layout: layout, Frames: 1000, NoiseAmp: 0.01, Seed: 8})
	same := true
	for i := range a {
		if a[i] != c[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("different seeds produced identical audio")
	}
}

func TestSignalSilentBlock(t *testing.T) {
	layout, _ := audio.ParseLayout("mono")
	blocks, _, _, err := Signal(SignalConfig{
		Layout: layout, Frames: 4096, BlockSize: 1024, Seed: 1, DropIndex: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range blocks {
		zero := true
		for _, v := range b.Data {
			if v != 0 {
				zero = false
				break
			}
		}
		if zero != (i == 2) {
			t.Fatalf("block %d zero=%v", i, zero)
		}
	}
}

func TestCorruptWAVIsDetectable(t *testing.T) {
	layout, _ := audio.ParseLayout("stereo")
	_, data, _, _ := Signal(SignalConfig{Layout: layout, Frames: 4800, BlockSize: 1024})
	bad := CorruptWAV(data)
	if len(bad) >= len(data) {
		t.Fatal("corruption did not shorten the file")
	}
	if _, err := audio.Parse(bad, "stereo"); err == nil {
		t.Fatal("corrupted WAV parsed cleanly")
	}
}

func TestVideoFrames(t *testing.T) {
	frames := VideoFrames(30, 30, 0, 0, 10)
	if len(frames) != 30 {
		t.Fatalf("count=%d", len(frames))
	}
	if frames[1].Tick != 3000 {
		t.Fatalf("frame1 tick=%d", frames[1].Tick)
	}
	if !frames[0].Key || !frames[10].Key || frames[11].Key {
		t.Fatal("keyframe cadence wrong")
	}
	drifted := VideoFrames(2, 30, 0, 10000, 0)
	if drifted[1].Tick <= frames[1].Tick {
		t.Fatal("drift did not advance timestamps")
	}
}

func TestWriteClipRoundTripsThroughProbe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clip.mp4")
	if err := WriteClip(ClipConfig{Path: path, Frames: 30, FPS: 30}); err != nil {
		t.Fatal(err)
	}
	tracks, duration, err := VerifyClip(path)
	if err != nil {
		t.Fatal(err)
	}
	if tracks != 1 || duration != 90000 {
		t.Fatalf("tracks=%d duration=%d", tracks, duration)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw[4:8]) != "ftyp" {
		t.Fatalf("missing ftyp: %q", raw[4:8])
	}
	// Locate mdat, then read the stco entry through go-mp4 and check that
	// the offset points exactly at the mdat payload.
	// stco sits nested inside moov/trak/mdia/minf/stbl, so search bytes
	// rather than walking the top-level box list.
	mdat := bytes.Index(raw, []byte("mdat")) - 4
	stco := bytes.LastIndex(raw, []byte("stco")) - 4
	if mdat < 0 || stco < 0 {
		t.Fatalf("boxes missing: mdat=%d stco=%d", mdat, stco)
	}
	if mdat == 0 || stco == 0 {
		t.Fatalf("boxes missing: mdat=%d stco=%d", mdat, stco)
	}
	// stco: size(4) type(4) version/flags(4) count(4) offset(4)
	off := binary.BigEndian.Uint32(raw[stco+16 : stco+20])
	if int(off) != mdat+8 {
		t.Fatalf("stco offset %d, want mdat payload %d", off, mdat+8)
	}
}

func TestWriteClipRejectsEmptyPath(t *testing.T) {
	if err := WriteClip(ClipConfig{}); err == nil {
		t.Fatal("empty path accepted")
	}
}
