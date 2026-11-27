package fixture

import "relayctl/internal/cmaf"

// damagePoint describes where a burst loss corrupts the fragment stream.
type damagePoint struct {
	// cutLength is how many bytes survive TruncateMdat.
	cutLength int
	// expectedOffset is the mdat box start the parser will anchor its
	// truncated-mdat BoxError on, guaranteeing an inspection delta of 0.
	expectedOffset int64
}

// locateDamage maps burst-lost RTP packets onto the mdat box they damage.
// Media packet n (stream order, excluding FEC) owns the mtu-sized chunk
// [n*mtu, (n+1)*mtu). The lowest lost chunk selects the affected mdat; the
// stream is cut inside that mdat while the manifest records the mdat start,
// which is exactly where the parser reports the first corrupted unit.
func locateDamage(stream []byte, burst []uint16) damagePoint {
	chunk := len(stream)
	if len(burst) > 0 {
		first := burst[0]
		for _, s := range burst[1:] {
			if s < first {
				first = s
			}
		}
		chunk = int(first) * mtu
	}

	boxes, err := cmaf.ScanBoxes(stream, 0)
	if err != nil {
		return damagePoint{cutLength: minInt(chunk, len(stream)), expectedOffset: int64(chunk)}
	}

	// Default to the first mdat if the chunk does not land inside one.
	var target cmaf.Box
	for _, b := range boxes {
		if b.Type == cmaf.TypeMdat {
			target = b
			break
		}
	}
	for _, b := range boxes {
		if b.Type != cmaf.TypeMdat {
			continue
		}
		payloadStart := int(b.Start) + b.HeaderSize
		payloadEnd := int(b.End())
		if chunk >= payloadStart && chunk < payloadEnd {
			target = b
			break
		}
	}
	if target.Type == "" {
		last := len(stream)
		return damagePoint{cutLength: last, expectedOffset: int64(last)}
	}

	payloadStart := int(target.Start) + target.HeaderSize
	payloadEnd := int(target.End())
	cut := chunk
	if cut < payloadStart+1 {
		cut = payloadStart + 1 // keep the mdat header, drop payload
	}
	if cut > payloadEnd-1 {
		cut = payloadEnd - 1
	}
	return damagePoint{cutLength: cut, expectedOffset: target.Start}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// mtu mirrors the packetization chunk size used by buildWire.
const mtu = 100
