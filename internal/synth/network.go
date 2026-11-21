package synth

// applyNetwork degrades the ideal capture deterministically: independent
// percentage rolls drive loss, local swaps drive reordering, and exact-copy
// duplicates exercise idempotent discard. RTCP reports are never dropped so
// calibration evidence always survives.
func (b *Builder) applyNetwork(recs []Record) []Record {
	cfg := b.cfg
	// 1. Duplication (before loss so a duplicate of a later-lost packet is
	// itself subject to the same loss roll semantics).
	var duped []Record
	for _, r := range recs {
		duped = append(duped, r)
		if r.Kind == 0 && b.pct(cfg.DuplicatePct) {
			duped = append(duped, r) // exact copy incl. sequence and timestamp
		}
	}
	// 2. Loss. RTCP and the first packet of each SSRC are protected so the
	// capture always starts parseable and calibratable.
	var kept []Record
	seenSSRC := map[uint32]bool{}
	for _, r := range duped {
		if r.Kind == 0 && seenSSRC[r.SSRC] && b.pct(cfg.LossPercent) {
			continue
		}
		seenSSRC[r.SSRC] = true
		kept = append(kept, r)
	}
	// 3. Reordering via bounded adjacent swaps; RTCP stays in place.
	for i := 1; i < len(kept); i++ {
		if kept[i].Kind == 0 && kept[i-1].Kind == 0 && b.pct(cfg.ReorderPercent) {
			kept[i], kept[i-1] = kept[i-1], kept[i]
		}
	}
	return kept
}

// pct rolls a 0..99 draw against a configured percentage.
func (b *Builder) pct(p int) bool {
	if p <= 0 {
		return false
	}
	if p >= 100 {
		return true
	}
	return b.rng.Intn(100) < p
}

// Truncate cuts the capture at keep records to emulate an interrupted tail.
func Truncate(recs []Record, keep int) []Record {
	if keep <= 0 || keep >= len(recs) {
		return recs
	}
	return append([]Record(nil), recs[:keep]...)
}
