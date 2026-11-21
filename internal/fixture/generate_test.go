package fixture

import (
	"bytes"
	"testing"

	"github.com/abema/go-mp4"
)

func testOpts() Options {
	return Options{
		Seed: 42, DurationSeconds: 3, VideoTimescale: 90000, VideoFPS: 25,
		AudioTimescale: 48000, AudioDelta: 9600, KeyEvery: 25, StartDTS: 1800,
		EditList: true, VideoBody: 16, AudioBody: 8,
		VideoStreamID: streamVideoBase, AudioStreamID: streamAudioBase,
	}
}

func TestGenerateDeterministic(t *testing.T) {
	a, la, err := GenerateMemory(testOpts())
	if err != nil {
		t.Fatal(err)
	}
	b, lb, err := GenerateMemory(testOpts())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("same seed must produce byte-identical assets")
	}
	if la.TotalSize != lb.TotalSize || int(la.TotalSize) != len(a.Bytes()) {
		t.Fatal("layout size mismatch")
	}
}

func TestSampleRoundTrip(t *testing.T) {
	raw := BuildSample(7, streamVideoBase, 33, 12)
	if n, err := VerifySample(raw, 33); err != nil || n != 12 {
		t.Fatalf("verify failed: n=%d err=%v", n, err)
	}
	raw[9] ^= 0xFF
	if _, err := VerifySample(raw, 33); err != ErrCRCMismatch {
		t.Fatalf("expected CRC mismatch, got %v", err)
	}
}

// TestParsableWithGoMp4 is the structural contract: the hand-written boxes
// must be accepted by the real go-mp4 parser used by internal/probe.
func TestParsableWithGoMp4(t *testing.T) {
	m, _, err := GenerateMemory(testOpts())
	if err != nil {
		t.Fatal(err)
	}
	var (
		sawMvhd bool
		sawElst bool
		elstN   []uint32
		stssN   int
		stszN   uint32
		stcoN   uint32
		trakCnt int
	)
	_, err = mp4.ReadBoxStructure(bytes.NewReader(m.Bytes()), func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type.String() {
		case "moov", "mdia", "minf", "stbl", "edts":
			return h.Expand()
		case "trak":
			trakCnt++
			return h.Expand()
		case "mvhd":
			sawMvhd = true
		case "elst":
			sawElst = true
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			if elst := box.(*mp4.Elst); elst.EntryCount < 1 || elst.EntryCount > 2 {
				t.Errorf("elst entry count = %d, want 1 or 2", elst.EntryCount)
			} else {
				elstN = append(elstN, elst.EntryCount)
			}
		case "stss":
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			stssN = len(box.(*mp4.Stss).SampleNumber)
		case "stsz":
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			stszN = box.(*mp4.Stsz).SampleCount
		case "stco":
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			stcoN = box.(*mp4.Stco).EntryCount
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("go-mp4 rejected fixture: %v", err)
	}
	if !sawMvhd || !sawElst || trakCnt != 2 {
		t.Fatalf("boxes missing: mvhd=%v elst=%v traks=%d", sawMvhd, sawElst, trakCnt)
	}
	var hasEmptyEdit bool
	for _, n := range elstN {
		if n == 2 {
			hasEmptyEdit = true
		}
	}
	if !hasEmptyEdit {
		t.Fatalf("video track must carry 2-entry edit list, got counts %v", elstN)
	}
	if stssN == 0 || stszN == 0 || stcoN == 0 {
		t.Fatalf("sample tables empty: stss=%d stsz=%d stco=%d", stssN, stszN, stcoN)
	}
}

func TestDamageInjection(t *testing.T) {
	m, lay, err := GenerateMemory(testOpts())
	if err != nil {
		t.Fatal(err)
	}
	orig := append([]byte(nil), m.Bytes()...)
	if err := CorruptSample(m, lay, 2); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(orig, m.Bytes()) {
		t.Fatal("corruption did not change bytes")
	}
	if err := TruncateTail(m, lay, 20); err != nil {
		t.Fatal(err)
	}
	if len(m.Bytes()) != int(lay.TotalSize)-20 {
		t.Fatal("truncate size wrong")
	}
}
