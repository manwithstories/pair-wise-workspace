package pipeline

import (
	"fmt"
	"io"
	"os"
	"sort"

	"archivecheck/internal/ebml"
	"archivecheck/internal/fault"
	"archivecheck/internal/fixture"
	"archivecheck/internal/index"
	"archivecheck/internal/timeline"
	"archivecheck/internal/transport"
	"archivecheck/internal/verify"
)

// runBuffer plans adaptive chunks from the bandwidth series and buffers the
// recovered payload under the configured memory budget.
func runBuffer(cfg Config, spillDir string, wire *fixture.Wire, res transport.Result, fs *fault.Set) (transport.SpillStats, error) {
	planCfg := transport.DefaultBudget(spillDir)
	planCfg.MemoryBytes = cfg.MemoryBudget
	chunks := transport.Plan(wire.Bandwidth, planCfg)
	buf := transport.NewBuffer(cfg.MemoryBudget, spillDir, fs)
	defer buf.Close()

	payload := res.Reassemble()
	if len(payload) == 0 {
		return buf.Stats(), nil
	}
	// Slice the recovered payload according to the planned chunk sizes so the
	// buffer sees exactly the adaptive sequence.
	pos := 0
	for _, c := range chunks {
		n := c.Bytes
		if n > len(payload)-pos {
			n = len(payload) - pos
		}
		if n < 0 {
			n = 0
		}
		if err := buf.Append(payload[pos : pos+n]); err != nil {
			return transport.SpillStats{}, err
		}
		pos += n
		if pos >= len(payload) {
			break
		}
	}
	if pos < len(payload) {
		if err := buf.Append(payload[pos:]); err != nil {
			return transport.SpillStats{}, err
		}
	}
	return buf.Stats(), nil
}

// fileVerifier re-reads the source at a candidate offset and confirms a real
// SimpleBlock/Block boundary with the expected payload.
type fileVerifier struct {
	path string
	size int64
}

func (v *fileVerifier) VerifyBlock(off int64) (index.BlockRef, error) {
	f, err := os.Open(v.path)
	if err != nil {
		return index.BlockRef{}, err
	}
	defer f.Close()
	if off < 0 || off >= v.size {
		return index.BlockRef{}, fmt.Errorf("offset 0x%x outside source", off)
	}
	r := ebml.NewReader(f, v.size, ebml.Options{})
	el, err := r.ReadElementAt(off)
	if err != nil {
		return index.BlockRef{}, fmt.Errorf("no element at 0x%x: %w", off, err)
	}
	if el.ID != ebml.IDSimpleBlock && el.ID != ebml.IDBlock {
		return index.BlockRef{}, fmt.Errorf("element at 0x%x is %s, not a block", off, boxName(el.ID))
	}
	payload, err := r.ReadPayload(el, 16)
	if err != nil {
		return index.BlockRef{}, err
	}
	if len(payload) < 4 {
		return index.BlockRef{}, fmt.Errorf("block at 0x%x has truncated header", off)
	}
	track, _, width, err := ebml.ParseTrackNumber(payload)
	if err != nil {
		return index.BlockRef{}, err
	}
	if 2+width >= len(payload) {
		return index.BlockRef{}, fmt.Errorf("block at 0x%x lacks flags", off)
	}
	flags := payload[2+width]
	key := el.ID == ebml.IDSimpleBlock && flags&0x80 != 0
	return index.BlockRef{
		Track:      track,
		Offset:     el.HeaderOffset,
		PayloadOff: el.PayloadOffset,
		PayloadLen: el.PayloadLen(),
		Element:    boxName(el.ID),
		Keyframe:   key,
	}, nil
}

func boxName(id ebml.ID) string {
	switch id {
	case ebml.IDSimpleBlock:
		return "SimpleBlock"
	case ebml.IDBlock:
		return "Block"
	}
	return fmt.Sprintf("0x%x", uint32(id))
}

// qualityStats compares recovered RTP payloads against the reference stream.
// Lost packets are counted as missing samples, never silently skipped.
func qualityStats(wire *fixture.Wire, res transport.Result) verify.QualityStats {
	gotBySeq := make(map[uint16][]byte, len(res.Packets))
	for _, p := range res.Packets {
		gotBySeq[p.Seq] = p.Payload
	}
	lost := make(map[uint16]bool)
	for _, g := range res.Gaps {
		for s := g.Start; s <= g.End; s++ {
			lost[uint16(s)] = true
		}
	}
	var samples []verify.Sample
	for _, ref := range wire.Reference {
		got, ok := gotBySeq[ref.Seq]
		if !ok || lost[ref.Seq] {
			// A lost packet is a real quality defect, not a sample to skip:
			// score its full payload against the maximum error so PSNR drops
			// as packets go missing instead of staying artificially perfect.
			samples = append(samples, verify.Sample{Missing: true})
			for i := 0; i+1 < len(ref.Payload); i += 2 {
				a := int16(ref.Payload[i]) | int16(ref.Payload[i+1])<<8
				samples = append(samples, verify.Sample{Ref: a, Got: -a})
			}
			continue
		}
		n := len(ref.Payload)
		if len(got) < n {
			n = len(got)
		}
		for i := 0; i+1 < n; i += 2 {
			a := int16(ref.Payload[i]) | int16(ref.Payload[i+1])<<8
			b := int16(got[i]) | int16(got[i+1])<<8
			samples = append(samples, verify.Sample{Ref: a, Got: b})
		}
	}
	return verify.PSNR(samples)
}

// computeBitrate compares the archive's visible payload rate against the rate
// the sealed source was built to carry.  Transport loss is accounted for by
// explicit missing slots, so it does not shrink the archive payload; source
// truncation does, which is what the 5% target guards.
func computeBitrate(plan *fixture.Plan, res *timeline.Result) verify.Bitrate {
	var bytes int64
	var durMs float64
	for _, tb := range verify.Tracks(res.Tracks, sortedTracks(res)) {
		bytes += tb.Bytes
		if tb.DurationMs > durMs {
			durMs = tb.DurationMs
		}
	}
	dur := plan.DurationSeconds
	if dur <= 0 {
		dur = durMs / 1000
	}
	target := int64(0)
	if dur > 0 {
		target = int64(float64(plan.PayloadBytes())*8/dur + 0.5)
	}
	return verify.ComputeBitrate(target, bytes, dur)
}

func sortedTracks(res *timeline.Result) []uint64 {
	out := make([]uint64, 0, len(res.Tracks))
	for n := range res.Tracks {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// copyFile is shared with stage writers.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
