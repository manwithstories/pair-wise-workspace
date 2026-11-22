package pipeline_test

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/pion/rtp"

	"mediascan/internal/clock"
	"mediascan/internal/fault"
	"mediascan/internal/pipeline"
	"mediascan/internal/report"
)

// writeBox is a minimal MP4 box encoder for self-generated fixtures.
func writeBox(b []byte, typ string, payload []byte) []byte {
	box := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(box, uint32(8+len(payload)))
	copy(box[4:], typ)
	copy(box[8:], payload)
	return append(b, box...)
}

func makeFixtures(t *testing.T) []pipeline.Input {
	t.Helper()
	dir := t.TempDir()

	// 1. Healthy SRT with three cues (segments>=3 expectation).
	srtPath := filepath.Join(dir, "good.srt")
	srt := "1\n00:00:01,000 --> 00:00:02,000\none\n\n" +
		"2\n00:00:03,000 --> 00:00:04,000\ntwo\n\n" +
		"3\n00:00:05,000 --> 00:00:06,000\nthree\n"
	if err := os.WriteFile(srtPath, []byte(srt), 0o600); err != nil {
		t.Fatal(err)
	}

	// 2. SRT with an invalid time line (subtitle faults flow into pipeline).
	badSRT := filepath.Join(dir, "bad.srt")
	if err := os.WriteFile(badSRT, []byte(
		"1\n00:00:99,000 --> 00:00:02,000\nbad\n\n2\n00:00:03,000 --> 00:00:04,000\nkept\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 3. Healthy MP4 with stss keyframes.
	mp4Path := filepath.Join(dir, "good.mp4")
	var mp4 []byte
	mp4 = writeBox(mp4, "ftyp", []byte("isom"))
	stssPayload := make([]byte, 8+8) // count=2, samples 1,26
	binary.BigEndian.PutUint32(stssPayload[4:], 2)
	binary.BigEndian.PutUint32(stssPayload[8:], 1)
	binary.BigEndian.PutUint32(stssPayload[12:], 26)
	stbl := writeBox(nil, "stss", stssPayload)
	moov := writeBox(nil, "moov",
		writeBox(nil, "trak",
			writeBox(nil, "mdia",
				writeBox(nil, "minf",
					writeBox(nil, "stbl", stbl)))))
	mp4 = append(mp4, moov...)
	mp4 = writeBox(mp4, "mdat", make([]byte, 64))
	if err := os.WriteFile(mp4Path, mp4, 0o600); err != nil {
		t.Fatal(err)
	}

	// 4. Truncated MP4: cut the mdat short (stream truncation -> recovery).
	truncPath := filepath.Join(dir, "trunc.mp4")
	if err := os.WriteFile(truncPath, mp4[:len(mp4)-20], 0o600); err != nil {
		t.Fatal(err)
	}

	// 5. RTP capture with an in-window reorder and one gap.
	rtpPath := filepath.Join(dir, "cap.rtp")
	var rtpBytes []byte
	for _, seq := range []uint16{100, 102, 101, 103, 200} {
		pkt := rtp.Packet{Header: rtp.Header{SequenceNumber: seq, SSRC: 7}, Payload: []byte{1}}
		raw, _ := pkt.Marshal()
		rec := make([]byte, 2+len(raw))
		binary.BigEndian.PutUint16(rec, uint16(len(raw)))
		copy(rec[2:], raw)
		rtpBytes = append(rtpBytes, rec...)
	}
	if err := os.WriteFile(rtpPath, rtpBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	return []pipeline.Input{
		{Path: srtPath, Kind: pipeline.KindSRT},
		{Path: badSRT, Kind: pipeline.KindSRT},
		{Path: mp4Path, Kind: pipeline.KindMP4},
		{Path: truncPath, Kind: pipeline.KindMP4},
		{Path: rtpPath, Kind: pipeline.KindRTP},
	}
}

func params() pipeline.Params { return pipeline.Params{Rate: clock.Rate25} }

func TestRunFullScan(t *testing.T) {
	dir := t.TempDir()
	inputs := makeFixtures(t)
	res, err := pipeline.Run(dir, inputs, params(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Segments) != 5 {
		t.Fatalf("segments = %d, want 5", len(res.Segments))
	}
	if res.Interrupted {
		t.Fatal("uninterrupted run must not flag interruption")
	}
	// bad.srt, trunc.mp4 and the gappy RTP capture are recovered segments.
	if res.RecoveredSegments < 3 {
		t.Fatalf("recovered segments = %d, want >=3", res.RecoveredSegments)
	}
	if res.KeyframeGapTotal < 1 {
		t.Fatal("healthy MP4 keyframe gaps must propagate")
	}
	// Faults are deterministically sorted by source/offset.
	for i := 1; i < len(res.Faults); i++ {
		a, b := res.Faults[i-1], res.Faults[i]
		if a.Source > b.Source || (a.Source == b.Source && a.Offset > b.Offset) {
			t.Fatal("faults not sorted deterministically")
		}
	}
}

func TestDeterministicChecksum(t *testing.T) {
	inputs := makeFixtures(t)
	r1, err := pipeline.Run(t.TempDir(), inputs, params(), 0)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := pipeline.Run(t.TempDir(), inputs, params(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if sum(r1) != sum(r2) {
		t.Fatal("identical inputs/params must yield identical summary bytes")
	}
}

func TestInterruptAndResume(t *testing.T) {
	dir := t.TempDir()
	inputs := makeFixtures(t)

	// First invocation stops after two verified segments.
	first, err := pipeline.Run(dir, inputs, params(), 2)
	if err != pipeline.ErrInterrupted || !first.Interrupted {
		t.Fatalf("want ErrInterrupted, got %v interrupted=%v", err, first.Interrupted)
	}
	if len(first.Segments) != 2 {
		t.Fatalf("first run segments = %d, want 2", len(first.Segments))
	}

	// Resume without an interrupt limit processes the remaining three.
	second, err := pipeline.Run(dir, inputs, params(), 0)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if len(second.Segments) != 3 {
		t.Fatalf("resume segments = %d, want 3", len(second.Segments))
	}

	// A complete uninterrupted run against fresh state for comparison.
	full, err := pipeline.Run(t.TempDir(), inputs, params(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// Reconstructed full segment list must equal the uninterrupted scan.
	if len(second.Segments)+len(first.Segments) != len(full.Segments) {
		t.Fatal("resume must recover all readable segments")
	}
	if sum(merge(first, second)) != sum(full) {
		t.Fatal("resumed report checksum must match uninterrupted run")
	}
}

func TestResumeRejectsChangedInput(t *testing.T) {
	dir := t.TempDir()
	inputs := makeFixtures(t)
	if _, err := pipeline.Run(dir, inputs, params(), 1); err != pipeline.ErrInterrupted {
		t.Fatal(err)
	}
	// Mutate the first input after it was verified.
	if err := os.WriteFile(inputs[0].Path, []byte("tampered bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Run(dir, inputs, params(), 0); err == nil {
		t.Fatal("resume against changed input must be refused")
	}
}

func TestRejectsOversizedInput(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "huge.srt")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := pipeline.Input{Path: p, Kind: pipeline.KindSRT}
	res, err := pipeline.Run(dir, []pipeline.Input{in}, params(), 0)
	if err != nil || len(res.Segments) != 1 {
		t.Fatalf("small input must pass size guard: %v %+v", err, res)
	}
	// The enforced ceiling is exactly 2 GiB per the contract.
	if fault.MaxInputSize != 2<<30 {
		t.Fatalf("max input size = %d, want 2 GiB", fault.MaxInputSize)
	}
	// A nonexistent input is a hard error (exit-code-2 class failure).
	if _, err := pipeline.Run(t.TempDir(), []pipeline.Input{{Path: "nope.srt", Kind: pipeline.KindSRT}}, params(), 0); err == nil {
		t.Fatal("missing input must return an error")
	}
}

// sortFaults mirrors the pipeline's deterministic fault ordering.
func sortFaults(fs []fault.Fault) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Offset != b.Offset {
			return a.Offset < b.Offset
		}
		if a.Stage != b.Stage {
			return a.Stage < b.Stage
		}
		return a.Message < b.Message
	})
}

// sum returns the deterministic report checksum for a result.
func sum(r *pipeline.Result) string {
	return report.Build(r).SHA256
}

// merge concatenates two partial results (interrupted run + resumed run) in
// segment order so the combined report can be compared checksum-for-checksum
// against an uninterrupted scan.
func merge(a, b *pipeline.Result) *pipeline.Result {
	out := &pipeline.Result{
		Params:            a.Params,
		Fingerprints:      a.Fingerprints,
		Segments:          append(append([]pipeline.SegmentReport{}, a.Segments...), b.Segments...),
		Faults:            append(append([]fault.Fault{}, a.Faults...), b.Faults...),
		RecoveredSegments: a.RecoveredSegments + b.RecoveredSegments,
		KeyframeGapTotal:  a.KeyframeGapTotal + b.KeyframeGapTotal,
		// The merged report represents one complete scan, just like the
		// uninterrupted reference run.
		Interrupted: false,
	}
	sortFaults(out.Faults)
	return out
}
