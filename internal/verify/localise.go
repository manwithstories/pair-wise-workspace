package verify

import (
	"sort"

	"relayaudit/internal/fault"
	"relayaudit/internal/transform"
	"relayaudit/internal/wire"
)

// measureLocalisation compares the reported CRC/truncation ranges with the
// injected damage.
func (r *Result) measureLocalisation(scan *wire.ScanResult, ranges []transform.Range, exp Expected) {
	var crc, trunc []transform.Range
	for _, rg := range ranges {
		switch rg.Kind {
		case fault.CRC:
			crc = append(crc, rg)
		case fault.Truncated:
			trunc = append(trunc, rg)
		}
	}
	if exp.CorruptInjected {
		if len(crc) == 0 {
			r.fail("injected CRC damage was not localised")
		} else {
			sort.Slice(crc, func(i, j int) bool { return crc[i].Start < crc[j].Start })
			r.CorruptOffsetDelta = abs64(crc[0].Start - exp.CorruptOffset)
			if r.CorruptOffsetDelta != 0 {
				r.fail("corrupt offset localised at %d, injected at %d (delta %d)",
					crc[0].Start, exp.CorruptOffset, r.CorruptOffsetDelta)
			}
		}
	}
	if exp.TruncateTail > 0 {
		if len(trunc) == 0 {
			r.fail("truncated tail was not isolated")
		} else {
			sort.Slice(trunc, func(i, j int) bool { return trunc[i].Start < trunc[j].Start })
			last := trunc[len(trunc)-1]
			r.TruncatedTailBytes = last.Missing
			if r.TruncatedTailBytes != int64(exp.TruncateTail) {
				r.fail("isolated tail is missing %d bytes, injected %d", r.TruncatedTailBytes, exp.TruncateTail)
			}
			if exp.SourceSize > 0 && last.End != exp.SourceSize {
				r.fail("tail range ends at %d, source ends at %d", last.End, exp.SourceSize)
			}
		}
	}
	if exp.DamageRanges >= 0 && r.DamageRanges != exp.DamageRanges {
		r.fail("damage_ranges %d, expected %d", r.DamageRanges, exp.DamageRanges)
	}
}
