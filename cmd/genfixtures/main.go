// Command genfixtures regenerates every scripted scenario and its expectation
// file under testdata/scenarios. Expectations are recomputed by feeding the
// scenario send plan through the real RTP depacketizer, so the fixtures never
// drift from the implementation.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	rtppkg "github.com/example/rtpcap/internal/rtp"
	"github.com/example/rtpcap/internal/scenario"
)

func main() {
	out := flag.String("out", "testdata/scenarios", "fixture output directory")
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}
	scenarios := []*scenario.Scenario{lossy(), clean(), resume(), authFail(), timeout()}
	for _, sc := range scenarios {
		scPath := filepath.Join(*out, sc.Name+".json")
		if err := sc.Save(scPath); err != nil {
			fatal(err)
		}
		ex := computeExpect(sc)
		exPath := filepath.Join(*out, sc.Name+".expect.json")
		if err := saveExpect(exPath, ex); err != nil {
			fatal(err)
		}
		fmt.Printf("wrote %s and %s (frames=%d gaps=%d/%d reorder=%d depth=%d)\n",
			scPath, exPath, ex.FramesMax, ex.SeqGapSegments, ex.GapPackets,
			ex.Reordered, ex.MaxReorderDepth)
	}
}

// lossy is the acceptance scenario: ~1200 packets with loss, reordering,
// duplicates, late packets and a mid-session cut.
func lossy() *scenario.Scenario {
	first := uint16(1000)
	last := uint16(1000 + 1200 - 1)
	sc := &scenario.Scenario{
		Name: "lossy", TotalPackets: 1200, PacketsPerFrame: 3,
		PayloadSize: 1330, StartSeq: 1000, SSRC: 0xCAFEBABE, ClockRate: 90000,
		Username: "inspector", Password: "s3cr3t", AuthEnabled: true,
		ReadTimeoutMS: 2500,
		Events: []scenario.Event{
			{Type: scenario.EventDrop, Pos: 120, Count: 5},
			{Type: scenario.EventDrop, Pos: 480, Count: 7},
			{Type: scenario.EventDrop, Pos: 900, Count: 5},
			{Type: scenario.EventReorder, Pos: 300, Shift: 8},
			{Type: scenario.EventReorder, Pos: 301, Shift: 8},
			{Type: scenario.EventReorder, Pos: 302, Shift: 8},
			{Type: scenario.EventReorder, Pos: 303, Shift: 8},
			{Type: scenario.EventReorder, Pos: 304, Shift: 8},
			{Type: scenario.EventReorder, Pos: 305, Shift: 8},
			{Type: scenario.EventReorder, Pos: 306, Shift: 8},
			{Type: scenario.EventReorder, Pos: 600, Shift: 8},
			{Type: scenario.EventReorder, Pos: 601, Shift: 8},
			{Type: scenario.EventReorder, Pos: 602, Shift: 8},
			{Type: scenario.EventReorder, Pos: 603, Shift: 8},
			{Type: scenario.EventReorder, Pos: 604, Shift: 8},
			{Type: scenario.EventReorder, Pos: 605, Shift: 8},
			{Type: scenario.EventReorder, Pos: 606, Shift: 8},
			{Type: scenario.EventDuplicate, Pos: 250, Count: 3},
			{Type: scenario.EventDuplicate, Pos: 610, Count: 3},
			{Type: scenario.EventDuplicate, Pos: 845, Count: 3},
			{Type: scenario.EventCut, Pos: 760, ResumeAt: 761},
		},
	}
	sc.Expect = scenario.Expect{
		StateTransitions: 6,
		RTPReceivedMin:   1190, RTPReceivedMax: 1210,
		SeqGapSegments: 3, FirstSeq: &first, LastSeq: &last,
		FramesMin: 395, FramesMax: 430,
		BytesMin: int64(13 * (1 << 20) / 10), BytesMax: int64(16 * (1 << 20) / 10),
	}
	return sc
}

// clean has no injections: used for throughput and correctness baselines.
func clean() *scenario.Scenario {
	sc := &scenario.Scenario{
		Name: "clean", TotalPackets: 600, PacketsPerFrame: 3,
		PayloadSize: 1200, StartSeq: 0, SSRC: 0x11111111, ClockRate: 90000,
		ReadTimeoutMS: 2500,
	}
	sc.Expect = scenario.Expect{
		StateTransitions: 6, RTPReceivedMin: 600, RTPReceivedMax: 600,
		FramesMin: 200, FramesMax: 200,
		BytesMin: 600 * 1200, BytesMax: 600 * 1200,
	}
	return sc
}

// resume exercises restart continuity: the client reconnects after a cut and
// resumes from the frame index without duplicating frames.
func resume() *scenario.Scenario {
	sc := &scenario.Scenario{
		Name: "resume", TotalPackets: 300, PacketsPerFrame: 3,
		PayloadSize: 1024, StartSeq: 500, SSRC: 0x22222222, ClockRate: 90000,
		ReadTimeoutMS: 2500,
		Events: []scenario.Event{
			{Type: scenario.EventCut, Pos: 140, ResumeAt: 141},
			{Type: scenario.EventDuplicate, Pos: 60, Count: 2},
		},
	}
	sc.Expect = scenario.Expect{
		StateTransitions: 6, RTPReceivedMin: 300, RTPReceivedMax: 305,
		FramesMin: 95, FramesMax: 100,
		BytesMin: 300 * 1024, BytesMax: 300 * 1024,
	}
	return sc
}

// authFail makes the server keep returning 401 at PLAY.
func authFail() *scenario.Scenario {
	sc := &scenario.Scenario{
		Name: "authfail", TotalPackets: 100, PacketsPerFrame: 2,
		PayloadSize: 512, StartSeq: 0, SSRC: 0x33333333, ClockRate: 90000,
		Username: "x", Password: "y", AuthEnabled: true, ReadTimeoutMS: 2500,
		Events: []scenario.Event{{Type: scenario.EventAuthFail}},
	}
	return sc
}

// timeout has the server stop sending early so the client read-deadline path
// can be exercised manually.
func timeout() *scenario.Scenario {
	sc := &scenario.Scenario{
		Name: "timeout", TotalPackets: 100, PacketsPerFrame: 2,
		PayloadSize: 512, StartSeq: 0, SSRC: 0x44444444, ClockRate: 90000,
		ReadTimeoutMS: 400,
		Events:        []scenario.Event{{Type: scenario.EventCut, Pos: 40, ResumeAt: -1}},
	}
	return sc
}

// computeExpect feeds the send plan through a real depacketizer and fills in
// the exact counters, preserving any hand-written range fields.
func computeExpect(sc *scenario.Scenario) scenario.Expect {
	dep := rtppkg.NewDepacketizer(64)
	for _, slot := range sc.BuildSendPlan() {
		if _, err := dep.Push(slot.Raw); err != nil {
			fatal(err)
		}
	}
	_ = dep.Flush()
	st, gaps := dep.Finalize()
	ex := sc.Expect
	ex.Duplicates = st.Duplicates
	ex.Reordered = st.Reordered
	ex.MaxReorderDepth = st.MaxReorderDepth
	ex.SeqGapSegments = st.GapSegments
	ex.GapPackets = st.GapPackets
	first := st.FirstSeq
	last := st.LastSeq
	ex.FirstSeq = &first
	ex.LastSeq = &last
	if ex.FramesMin == 0 || ex.FramesMax == 0 {
		ex.FramesMin, ex.FramesMax = st.Frames, st.Frames
	}
	if ex.BytesMin == 0 || ex.BytesMax == 0 {
		ex.BytesMin, ex.BytesMax = st.Bytes, st.Bytes
	}
	if ex.RTPReceivedMin == 0 || ex.RTPReceivedMax == 0 {
		ex.RTPReceivedMin, ex.RTPReceivedMax = st.Received, st.Received
	}
	_ = gaps
	return ex
}

func saveExpect(path string, ex scenario.Expect) error {
	b, err := json.MarshalIndent(ex, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "genfixtures:", err)
	os.Exit(1)
}
