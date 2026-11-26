package transport

import (
	"crypto/sha256"
	"testing"

	"avpipe/internal/media"
)

func videoPacket(seq uint32, payload []byte) *media.Packet {
	block := seq / BlockSize
	wrapped := WrapPayload(media.KindVideo, seq, media.VideoPTS(seq), payload)
	sum := sha256.Sum256(wrapped)
	return &media.Packet{Seq: seq, BlockID: block, Kind: media.KindVideo,
		PTS: media.VideoPTS(seq), Payload: wrapped, SHA256: sum[:]}
}

func buildVideoBlock(block uint32, sizes []int) []*media.Packet {
	var pkts []*media.Packet
	members := make([][]byte, BlockSize)
	for slot := 0; slot < BlockSize; slot++ {
		n := BlockSize
		if slot < len(sizes) {
			n = sizes[slot]
		}
		payload := make([]byte, n)
		for i := range payload {
			payload[i] = byte(int(seqOf(block, slot)) + i%7)
		}
		p := videoPacket(seqOf(block, slot), payload)
		pkts = append(pkts, p)
		members[slot] = p.Payload
	}
	parity := Parity(members)
	pkts = append(pkts, &media.Packet{
		Seq: block*BlockSize + BlockSize, BlockID: block, Kind: media.KindParity,
		ParityOf: media.KindVideo, PTS: media.VideoPTS(block * BlockSize), Payload: parity,
	})
	return pkts
}

func seqOf(block uint32, slot int) uint32 { return block*BlockSize + uint32(slot) }

func TestFECSingleLossRecovery(t *testing.T) {
	original := buildVideoBlock(0, nil)
	want := original[3]
	delivered := append([]*media.Packet{}, original[:3]...)
	delivered = append(delivered, original[4:]...) // drop slot 3

	out, stats, errs := DecodeFEC(delivered)
	if stats.Recovered != 1 {
		t.Fatalf("Recovered = %d, want 1 (errs=%v)", stats.Recovered, errs)
	}
	if len(stats.GapSlots) != 0 {
		t.Fatalf("unexpected gap slots: %d", len(stats.GapSlots))
	}
	var rec *media.Packet
	for _, p := range out {
		if p.Seq == want.Seq && p.Kind == media.KindVideo {
			rec = p
		}
	}
	if rec == nil {
		t.Fatal("recovered packet missing from output")
	}
	if !rec.Recovered {
		t.Error("recovered packet must set Recovered flag")
	}
	kind, seq, pts, content, ok := UnwrapPayload(rec.Payload)
	if !ok || kind != media.KindVideo || seq != want.Seq || pts != want.PTS {
		t.Fatalf("recovered header mismatch: %+v", rec)
	}
	_, _, _, wantContent, _ := UnwrapPayload(want.Payload)
	if string(content) != string(wantContent) {
		t.Error("recovered content differs from original")
	}
}

func TestFECBurstLossBecomesGap(t *testing.T) {
	original := buildVideoBlock(2, nil)
	var delivered []*media.Packet
	for i, p := range original {
		if i == 2 || i == 3 || p.Kind == media.KindParity {
			continue // two media packets plus parity gone
		}
		delivered = append(delivered, p)
	}
	out, stats, _ := DecodeFEC(delivered)
	if stats.BurstLossBlocks != 1 {
		t.Fatalf("BurstLossBlocks = %d, want 1", stats.BurstLossBlocks)
	}
	if len(stats.GapSlots) != 2 {
		t.Fatalf("GapSlots = %d, want 2", len(stats.GapSlots))
	}
	if stats.Recovered != 0 {
		t.Fatalf("Recovered = %d, want 0", stats.Recovered)
	}
	for _, p := range out {
		if p.Seq == seqOf(2, 2) || p.Seq == seqOf(2, 3) {
			t.Error("burst-lost packets must not reappear")
		}
	}
}

func TestParityRequiresAllButOne(t *testing.T) {
	original := buildVideoBlock(0, nil)
	members := make([][]byte, 0, BlockSize)
	for slot := 0; slot < BlockSize; slot++ {
		members = append(members, original[slot].Payload)
	}
	parity := Parity(members)
	// Recover slot 5 using parity XOR the other seven members.
	others := append([][]byte{}, members[:5]...)
	others = append(others, members[6:]...)
	got := make([]byte, len(parity))
	copy(got, parity)
	for _, m := range others {
		for i := range m {
			got[i] ^= m[i]
		}
	}
	if string(got) != string(members[5]) {
		t.Error("manual XOR reconstruction failed")
	}
}

func TestReorderDedupAndConflict(t *testing.T) {
	a := videoPacket(0, []byte{1, 2, 3})
	b := videoPacket(1, []byte{4, 5, 6})
	c := videoPacket(2, []byte{7, 8, 9})
	// Deliver out of order, with an exact duplicate of b and a conflicting
	// duplicate of c carrying different payload bytes.
	conflict := *c
	conflict.Payload = WrapPayload(media.KindVideo, 2, c.PTS, []byte{9, 9, 9})
	in := []*media.Packet{c, a, b, b, &conflict}

	ordered, dedup, dedupErrs := ReorderDedup(in)
	if dedup.ExactDuplicates != 1 {
		t.Errorf("ExactDuplicates = %d, want 1", dedup.ExactDuplicates)
	}
	if dedup.ConflictingDuplicates != 1 {
		t.Errorf("ConflictingDuplicates = %d, want 1", dedup.ConflictingDuplicates)
	}
	if len(ordered) != 3 {
		t.Fatalf("ordered length = %d, want 3", len(ordered))
	}
	if ordered[0].Seq != 0 || ordered[1].Seq != 1 || ordered[2].Seq != 2 {
		t.Errorf("not sorted by PTS: %d %d %d", ordered[0].Seq, ordered[1].Seq, ordered[2].Seq)
	}
	if dedup.Reordered < 1 {
		t.Error("out-of-order delivery was not counted")
	}
	var dupErr *media.Error
	for _, e := range dedupErrs {
		if e.Class == media.ClassDuplicate {
			dupErr = e
		}
	}
	if dupErr == nil {
		t.Fatalf("conflict must produce a duplicate-class error, got %v", dedupErrs)
	}
}

func TestWraparoundDetection(t *testing.T) {
	near := videoPacket(0, []byte{1})
	near.PTS = media.PTSModulo - media.ClockRate/media.VideoFPS
	after := videoPacket(1, []byte{2})
	after.PTS = media.ClockRate / media.VideoFPS // wrapped back near zero
	_, stats, errs := ReorderDedup([]*media.Packet{near, after})
	if stats.Wraparounds != 1 {
		t.Fatalf("Wraparounds = %d, want 1", stats.Wraparounds)
	}
	var clockErr *media.Error
	for _, e := range errs {
		if e.Class == media.ClassClock {
			clockErr = e
		}
	}
	if clockErr == nil {
		t.Fatalf("expected a clock error, got %v", errs)
	}
}

func TestVideoAudioBlocksAreIndependent(t *testing.T) {
	v := buildVideoBlock(0, nil)
	audio := make([]*media.Packet, 0, BlockSize+1)
	members := make([][]byte, BlockSize)
	for slot := 0; slot < BlockSize; slot++ {
		wrapped := WrapPayload(media.KindAudio, uint32(slot),
			media.AudioPTS(slot*media.AudioSamplesPerPacket), make([]byte, 32))
		members[slot] = wrapped
		audio = append(audio, &media.Packet{Seq: uint32(slot), BlockID: 0,
			Kind: media.KindAudio, PTS: media.AudioPTS(slot * media.AudioSamplesPerPacket),
			Payload: wrapped})
	}
	audio = append(audio, &media.Packet{Seq: BlockSize, BlockID: 0,
		Kind: media.KindParity, ParityOf: media.KindAudio, Payload: Parity(members)})

	// Drop video slot 1 and audio slot 6: both must recover independently.
	delivered := append([]*media.Packet{}, v[:1]...)
	delivered = append(delivered, v[2:]...)
	for i, p := range audio {
		if i == 6 {
			continue
		}
		delivered = append(delivered, p)
	}
	_, stats, errs := DecodeFEC(delivered)
	if stats.Recovered != 2 {
		t.Fatalf("Recovered = %d, want 2 (errs=%v)", stats.Recovered, errs)
	}
}
