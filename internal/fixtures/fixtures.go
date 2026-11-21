// Package fixtures generates every sample input the pipeline needs: metadata
// containers and RTP packet streams covering the success path, wrap,
// duplicates, reordering, burst loss and an interrupted run. Nothing is read
// from external files or services.
package fixtures

import (
	"fmt"

	"github.com/pion/rtp"

	"mediad/internal/metadata"
	"mediad/internal/model"
)

// Scenario names.
const (
	ScenarioSuccess   = "success"
	ScenarioCorrupt   = "corrupt"
	ScenarioWrap      = "wrap"
	ScenarioDuplicate = "duplicate"
	ScenarioReorder   = "reorder"
	ScenarioBurst     = "burst-loss"
	ScenarioInterrupt = "interrupt"
)

// AllScenarios lists every generated fixture in stable order.
func AllScenarios() []string {
	return []string{ScenarioSuccess, ScenarioCorrupt, ScenarioWrap, ScenarioDuplicate,
		ScenarioReorder, ScenarioBurst, ScenarioInterrupt}
}

// PacketPlan describes the impairments applied to a generated RTP stream.
type PacketPlan struct {
	Count int
	// Drop is the set of packet indices to omit entirely.
	Drop map[int]bool
	// BurstStart/BurstLen omit a contiguous run (added to Drop).
	BurstStart, BurstLen int
	// Duplicate indices are emitted twice.
	Duplicate map[int]bool
	// Reorder swaps the arrival order of selected adjacent pairs.
	Reorder []int
	// StartSeq is the first 16-bit sequence number; wrap scenarios set it near
	// 65535 so the extension rule is exercised.
	StartSeq uint16
	// PayloadBytes per packet.
	PayloadBytes int
	// TimestampStep in RTP units.
	TimestampStep uint32
}

// Packet is one planned packet.
type Packet struct {
	Seq       uint16
	Timestamp uint32
	Index     int
	// Duplicate marks a repeat transmission of an earlier index.
	Duplicate bool
}

// BuildPackets returns the emission order for a plan, including duplicates and
// reordering, plus the set of omitted indices.
func BuildPackets(plan PacketPlan) ([]Packet, []int) {
	if plan.PayloadBytes <= 0 {
		plan.PayloadBytes = 200
	}
	if plan.TimestampStep == 0 {
		plan.TimestampStep = 3000
	}
	var planned []Packet
	var lost []int
	drop := map[int]bool{}
	for i := range plan.Drop {
		drop[i] = true
	}
	for i := 0; i < plan.BurstLen; i++ {
		drop[plan.BurstStart+i] = true
	}
	for i := 0; i < plan.Count; i++ {
		if drop[i] {
			lost = append(lost, i)
			continue
		}
		seq := plan.StartSeq + uint16(i)
		planned = append(planned, Packet{Seq: seq, Index: i, Timestamp: uint32(i) * plan.TimestampStep})
	}
	// Apply duplicates by re-emitting the prior packet adjacent to its first
	// transmission (the classic "duplicate" impairment).
	var withDups []Packet
	for _, p := range planned {
		withDups = append(withDups, p)
		if plan.Duplicate[p.Index] {
			d := p
			d.Duplicate = true
			withDups = append(withDups, d)
		}
	}
	// Apply reordering by swapping the arrival order of the listed pairs.
	for _, i := range plan.Reorder {
		if i+1 < len(withDups) {
			withDups[i], withDups[i+1] = withDups[i+1], withDups[i]
		}
	}
	return withDups, lost
}

// EncodePackets marshals planned packets with pion/rtp. Payloads are
// deterministic per index so frame reconstruction can be verified.
func EncodePackets(plan PacketPlan, packets []Packet, ssrc uint32) ([][]byte, error) {
	if plan.PayloadBytes <= 0 {
		plan.PayloadBytes = 200
	}
	out := make([][]byte, 0, len(packets))
	for _, p := range packets {
		payload := make([]byte, plan.PayloadBytes)
		for i := range payload {
			payload[i] = byte((p.Index*31 + i*7) & 0xFF)
		}
		pkt := rtp.Packet{
			Header: rtp.Header{
				Version: 2, SequenceNumber: p.Seq, Timestamp: p.Timestamp,
				SSRC: ssrc, PayloadType: 96,
				Marker: p.Index == plan.Count-1,
			},
			Payload: payload,
		}
		raw, err := pkt.Marshal()
		if err != nil {
			return nil, model.Wrap(model.CategoryTransport, "fixture-encode", err)
		}
		out = append(out, raw)
	}
	return out, nil
}

// DefaultPlan is the acceptance-path plan: no loss, no duplicates, no reorder.
func DefaultPlan(count int) PacketPlan {
	return PacketPlan{Count: count, PayloadBytes: 200, TimestampStep: 3000, StartSeq: 100}
}

// ScenarioPlan returns the impairment plan for a named scenario.
func ScenarioPlan(name string, count int, seed int64) (PacketPlan, error) {
	plan := DefaultPlan(count)
	_ = seed // scenario impairments are deliberate; the seed selects payloads
	switch name {
	case ScenarioSuccess:
		return plan, nil
	case ScenarioWrap:
		plan.StartSeq = 65530 // wraps after 6 packets
		return plan, nil
	case ScenarioDuplicate:
		plan.Duplicate = map[int]bool{}
		for i := 0; i < count; i += 7 {
			plan.Duplicate[i] = true
		}
		return plan, nil
	case ScenarioReorder:
		// Swap a few adjacent arrivals, always inside the reorder window.
		for i := 3; i+1 < count; i += 5 {
			plan.Reorder = append(plan.Reorder, i)
		}
		return plan, nil
	case ScenarioBurst:
		plan.BurstStart = count / 3
		plan.BurstLen = 4
		return plan, nil
	case ScenarioInterrupt:
		// The stream itself is clean; the interrupt is modelled by the
		// scheduler resume fixture, not by packet loss.
		return plan, nil
	case ScenarioCorrupt:
		return plan, nil
	default:
		return PacketPlan{}, model.New(model.CategoryUsage, "fixture", "unknown scenario "+name)
	}
}

// WriteMetadataPair writes sample.mp4 and sample.mxf into dir.
func WriteMetadataPair(dir string, corrupt bool) (mp4Path, mxfPath string, err error) {
	mp4Path, err = metadata.WriteDemoMP4(dir, corrupt)
	if err != nil {
		return "", "", err
	}
	mxfPath, err = metadata.WriteDemoMXF(dir, corrupt)
	if err != nil {
		return "", "", err
	}
	return mp4Path, mxfPath, nil
}

// Stream is a fully encoded fixture stream.
type Stream struct {
	Scenario string
	Plan     PacketPlan
	Packets  [][]byte
	// LostIndices are the packet indices intentionally omitted.
	LostIndices []int
	// DuplicateCount is how many extra packets were emitted.
	DuplicateCount int
}

// BuildStream builds the encoded stream for a scenario.
func BuildStream(name string, count int, seed int64) (Stream, error) {
	plan, err := ScenarioPlan(name, count, seed)
	if err != nil {
		return Stream{}, err
	}
	packets, lost := BuildPackets(plan)
	raw, err := EncodePackets(plan, packets, 0xDEADBEEF)
	if err != nil {
		return Stream{}, err
	}
	dups := 0
	for _, p := range packets {
		if p.Duplicate {
			dups++
		}
	}
	return Stream{Scenario: name, Plan: plan, Packets: raw, LostIndices: lost, DuplicateCount: dups}, nil
}

// Describe renders a human-readable fixture summary.
func (s Stream) Describe() string {
	return fmt.Sprintf("%s: %d packets, %d lost, %d duplicates", s.Scenario, len(s.Packets), len(s.LostIndices), s.DuplicateCount)
}
