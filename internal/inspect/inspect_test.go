package inspect

import (
	"bytes"
	"io"
	"testing"

	"tsbatch/internal/fault"
	"tsbatch/internal/sample"
	"tsbatch/internal/ts"
)

// sinkRecorder records the ordered sink stream.
type sinkRecorder struct {
	segments   [][2]uint64
	isolations [][2]uint64
	appended   int
	open       bool
}

func (s *sinkRecorder) BeginSegment(id int, first uint64, t int64) { s.open = true }
func (s *sinkRecorder) Append(ev PacketEvent) {
	if !s.open {
		panic("append outside segment")
	}
	s.appended++
}
func (s *sinkRecorder) EndSegment(id int, last uint64, t int64) {
	s.segments = append(s.segments, [2]uint64{0, last})
	s.open = false
}
func (s *sinkRecorder) Isolate(start, end uint64, r fault.Category) {
	s.isolations = append(s.isolations, [2]uint64{start, end})
}

// feed parses every packet of stream and runs it through the analyzer.
func feed(t *testing.T, stream []byte, an *Analyzer) uint64 {
	t.Helper()
	rd, err := ts.NewReader(bytes.NewReader(stream), ts.PacketSize)
	if err != nil {
		t.Fatal(err)
	}
	var pkt [ts.PacketSize]byte
	var last uint64
	for {
		idx, err := rd.Next(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			if _, ok := err.(*ts.TruncationError); ok {
				break
			}
			t.Fatal(err)
		}
		ev, err := ParsePacket(idx, &pkt)
		if err != nil {
			t.Fatal(err)
		}
		an.Consume(ev)
		last = idx
	}
	return last
}

func TestAnalyzerCleanStream(t *testing.T) {
	stream := sample.Generate(sample.Options{Packets: 2000})
	sink := &sinkRecorder{}
	an := NewAnalyzer(sink, 800)
	last := feed(t, stream, an)
	rep := an.Finalize(last)

	if len(rep.Faults) != 0 {
		t.Fatalf("clean stream must have no faults: %v", rep.Faults)
	}
	if len(rep.Programs) != 1 || rep.Programs[0].PCRPID != sample.PIDVideo {
		t.Fatalf("programs=%+v", rep.Programs)
	}
	if len(rep.Programs[0].Streams) != 2 {
		t.Fatalf("streams=%+v", rep.Programs[0].Streams)
	}
	if rep.PCRWraps != 0 || rep.PSIUpdates != 0 {
		t.Fatalf("wraps=%d updates=%d", rep.PCRWraps, rep.PSIUpdates)
	}
	if len(rep.Keyframes) == 0 || rep.MaxKFInterval != 90000 {
		t.Fatalf("keyframes=%d maxInterval=%d", len(rep.Keyframes), rep.MaxKFInterval)
	}
	if rep.Segments < 2 || sink.appended == 0 {
		t.Fatalf("segments=%d appended=%d", rep.Segments, sink.appended)
	}
	// Keyframe times must be monotonic on the unified timeline.
	for i := 1; i < len(rep.Keyframes); i++ {
		if rep.Keyframes[i].Time <= rep.Keyframes[i-1].Time {
			t.Fatal("keyframe times must be monotonic")
		}
	}
}

func TestAnalyzerFaultsAndRecovery(t *testing.T) {
	stream := sample.Generate(sample.Options{
		Packets: 4000, PCRWrap: true,
		CCGapAt: 1500, CCGapSize: 3,
		PSIUpdateAt: 2000, TruncateBytes: 37,
	})
	sink := &sinkRecorder{}
	an := NewAnalyzer(sink, 1000)
	last := feed(t, stream, an)
	rep := an.Finalize(last)

	if rep.PCRWraps != 1 {
		t.Fatalf("pcr wraps=%d want 1", rep.PCRWraps)
	}
	if rep.PSIUpdates != 1 {
		t.Fatalf("psi updates=%d want 1", rep.PSIUpdates)
	}
	gaps := rep.Faults.Count(fault.CatCCGap)
	if gaps != 1 {
		t.Fatalf("cc gaps=%d want 1 (%v)", gaps, rep.Faults)
	}
	if len(rep.Isolations) != 1 || len(sink.isolations) != 1 {
		t.Fatalf("isolations=%v", rep.Isolations)
	}
	iso := rep.Isolations[0]
	if iso.Reason != fault.CatCCGap || iso.EndPacket < iso.StartPacket {
		t.Fatalf("bad isolation %+v", iso)
	}
	// The segment after the gap must resume at a keyframe.
	resume := iso.EndPacket + 1
	isKF := false
	for _, kf := range rep.Keyframes {
		if kf.PacketIndex == resume {
			isKF = true
		}
	}
	if !isKF {
		t.Fatalf("segment must resume at a keyframe, resume=%d", resume)
	}
	// Faults carry the unified media time once PCR is known.
	for _, f := range rep.Faults {
		if f.Category == fault.CatCCGap && f.MediaTime == fault.UnknownTime {
			t.Fatal("cc gap fault must carry media time")
		}
	}
}

func TestParsePacketPTS(t *testing.T) {
	stream := sample.Generate(sample.Options{Packets: 100})
	rd, _ := ts.NewReader(bytes.NewReader(stream), ts.PacketSize)
	var pkt [ts.PacketSize]byte
	foundPTS := false
	for i := 0; i < 100; i++ {
		idx, err := rd.Next(&pkt)
		if err != nil {
			t.Fatal(err)
		}
		ev, err := ParsePacket(idx, &pkt)
		if err != nil {
			t.Fatal(err)
		}
		if ev.Header.PID == sample.PIDVideo && ev.Header.PUSI {
			if !ev.HasPTS {
				t.Fatal("video PUSI packet must carry PTS")
			}
			foundPTS = true
		}
	}
	if !foundPTS {
		t.Fatal("no video PUSI packet found")
	}
}
