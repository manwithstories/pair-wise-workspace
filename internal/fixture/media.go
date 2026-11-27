package fixture

import (
	"fmt"
	"math/rand"

	"relayaudit/internal/wire"
)

// applyDamage mutates out in place, fills in the Damage report and returns the
// possibly shortened live view of the file.
func applyDamage(out []byte, seg *Segment, spec Spec) ([]byte, error) {
	d := &seg.Damage
	if spec.CorruptRecord > 0 {
		if spec.CorruptRecord > len(seg.Records) {
			return nil, fmt.Errorf("corrupt record %d exceeds record count %d", spec.CorruptRecord, len(seg.Records))
		}
		rec := seg.Records[spec.CorruptRecord-1]
		pos := rec.Offset + wire.RecordHeaderSize + int64(len(rec.Payload))/2
		out[pos] ^= 0x5a
		d.CorruptRecord = spec.CorruptRecord
		d.CorruptRecordOff = rec.Offset
		d.CorruptByteOff = pos
	}
	if spec.DropIndexPage > 0 {
		pages := wire.PagesFor(len(seg.Records))
		if spec.DropIndexPage > pages {
			return nil, fmt.Errorf("drop page %d exceeds page count %d", spec.DropIndexPage, pages)
		}
		pageOff := int64(wire.HeaderSize) + int64(spec.DropIndexPage-1)*wire.PageSize
		clear(out[pageOff : pageOff+wire.PageSize])
		d.DropIndexPage = spec.DropIndexPage
		d.DroppedPageOffset = pageOff
	}
	if spec.TruncateTail > 0 {
		if spec.TruncateTail >= len(out) {
			return nil, fmt.Errorf("truncate tail %d exceeds file size %d", spec.TruncateTail, len(out))
		}
		d.TruncateTail = spec.TruncateTail
		out = out[:len(out)-spec.TruncateTail]
	}
	return out, nil
}

// payloadSize returns the exact payload length for a track at a given tick.
func payloadSize(trackIdx int, ticks uint64) int {
	switch trackIdx {
	case 0:
		return 2400 + int(ticks%600)
	case 1:
		return 1800 + int(ticks%400)
	case 2:
		return 360 + int(ticks%80)
	default:
		return 300 + int(ticks%60)
	}
}

// buildPayload builds exactly payloadSize bytes of synthetic media for track.
func buildPayload(rnd *rand.Rand, trackIdx int, ticks uint64) []byte {
	target := payloadSize(trackIdx, ticks)
	if trackIdx >= 2 {
		return audioFrame(rnd, target)
	}
	return videoFrame(rnd, target, ticks)
}

// videoFrame emits an Annex B access unit. IDR frames lead with SPS and PPS
// NALs and use NAL type 5; all other frames use a single non-IDR slice.
func videoFrame(rnd *rand.Rand, target int, ticks uint64) []byte {
	isIDR := int(ticks/videoFrameTicks)%idrEveryFrames == 0
	out := make([]byte, 0, target)
	if isIDR {
		out = appendNAL(out, 7, rnd, 12) // SPS
		out = appendNAL(out, 8, rnd, 8)  // PPS
	}
	remaining := target - len(out)
	if remaining < 5 {
		remaining = 5
	}
	nalType := byte(1)
	if isIDR {
		nalType = 5
	}
	out = appendNAL(out, nalType, rnd, remaining-5)
	if len(out) != target {
		panic("fixture: video payload size drift")
	}
	return out
}

func appendNAL(dst []byte, nalType byte, rnd *rand.Rand, body int) []byte {
	dst = append(dst, 0x00, 0x00, 0x00, 0x01)
	// forbidden_zero=0, nal_ref_idc=3 for parameter sets and IDRs, 2 otherwise.
	refIDC := byte(0x40)
	if nalType == 5 || nalType == 7 || nalType == 8 {
		refIDC = 0x60
	}
	dst = append(dst, refIDC|nalType)
	if body <= 0 {
		return dst
	}
	buf := make([]byte, body)
	rnd.Read(buf)
	return append(dst, buf...)
}

// audioFrame emits one ADTS frame of exactly target bytes.
func audioFrame(rnd *rand.Rand, target int) []byte {
	out := make([]byte, target)
	out[0] = 0xFF
	out[1] = 0xF1                           // MPEG-4, layer 0, no CRC
	out[2] = (1 << 6) | (3 << 2) | (1 >> 2) // AAC-LC, 48 kHz, 2 channels
	out[3] = ((1 & 0x03) << 6) | byte(target>>11)
	out[4] = byte(target >> 3)
	out[5] = byte((target&0x07)<<5) | 0x1F
	out[6] = 0xFC
	rnd.Read(out[7:])
	return out
}
