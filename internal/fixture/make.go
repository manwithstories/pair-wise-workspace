package fixture

import (
	"fmt"
	"math/rand"

	"mediad/internal/ingest"
)

const (
	DemoSegments      = 8
	segmentTicks      = 36000 // 400 ms common aligned interval
	aacFrameTicks     = 1920  // 1024 samples at 48 kHz in 90 kHz units
	damagePacketIndex = 37
)

// RenditionSpec controls deterministic bitrate/size variation.
type RenditionSpec struct {
	Index     int
	ID        string
	Bandwidth int
	Width     int
	Height    int
	Filler    int
}

// DemoRenditionSpecs are the two different-bitrate representations.
func DemoRenditionSpecs() []RenditionSpec {
	return []RenditionSpec{
		{Index: 0, ID: "video_800k", Bandwidth: 800_000, Width: 640, Height: 360, Filler: 24},
		{Index: 1, ID: "video_2400k", Bandwidth: 2_400_000, Width: 1280, Height: 720, Filler: 96},
	}
}

type mediaItem struct {
	pid     uint16
	stream  byte
	pts     uint64
	key     bool
	payload []byte
	pcr     bool
}

// CleanRendition generates one valid clean H.264/AAC TS byte stream.
func CleanRendition(seed int64, spec RenditionSpec) ([]byte, error) {
	r := rand.New(rand.NewSource(seed + int64(spec.Index)*100003))
	items := buildMediaItems(seed, spec, r)
	w := NewWriter()
	w.WriteSection(PATPID, PAT(0, PMTPID))
	w.WriteSection(PMTPID, PMT(0))
	for _, item := range items {
		var pcr *ingest.PCR
		if item.pcr {
			pcr = &ingest.PCR{Base: item.pts, Extension: 0}
		}
		if err := w.WritePES(item.pid, item.stream, item.pts, item.payload, pcr); err != nil {
			return nil, err
		}
	}
	if len(w.Buf)%ingest.PacketSize != 0 {
		return nil, fmt.Errorf("fixture produced partial TS: %d bytes", len(w.Buf))
	}
	return w.Buf, nil
}

func buildMediaItems(seed int64, spec RenditionSpec, r *rand.Rand) []mediaItem {
	var items []mediaItem
	lastBoundary := uint64(DemoSegments * segmentTicks)
	// IDR boundaries 0..DemoSegments provide DemoSegments closed intervals.
	for b := 0; b <= DemoSegments; b++ {
		pts := uint64(b * segmentTicks)
		items = append(items, mediaItem{
			pid: VideoPID, stream: 0xe0, pts: pts, key: true, pcr: true,
			payload: h264KeyAccessUnit(seed, b, spec.Filler),
		})
		if b == DemoSegments {
			break
		}
		// Variable frame durations: alternate irregular 90k ticks rather than
		// quantizing every frame to one nominal FPS.
		patterns := []uint64{3003, 3003, 4504, 2997, 3604, 2892}
		pts += patterns[b%len(patterns)]
		k := 0
		for pts < uint64((b+1)*segmentTicks) {
			items = append(items, mediaItem{
				pid: VideoPID, stream: 0xe0, pts: pts, key: false,
				payload: h264PFrame(seed, b*100+k, spec.Filler),
			})
			step := patterns[(b+k+1)%len(patterns)]
			pts += step
			k++
		}
	}
	// Continuous AAC ADTS frames across the complete timeline.
	frameLen := 32 + r.Intn(16)
	for pts := uint64(0); pts < lastBoundary; pts += aacFrameTicks {
		items = append(items, mediaItem{
			pid: AudioPID, stream: 0xc0, pts: pts,
			payload: aacFrame(seed, int(pts/aacFrameTicks), frameLen),
		})
	}
	// Stable mux order by PTS, with video before audio at identical PTS.
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j-1].pts > items[j].pts; j-- {
			items[j-1], items[j] = items[j], items[j-1]
		}
	}
	return items
}

func h264KeyAccessUnit(seed int64, index, filler int) []byte {
	var b []byte
	b = append(b, 0, 0, 0, 1, 0x67, 0x42, 0xc0, 0x1e, 0xd9, 0x01, 0x40, 0x7b) // SPS-ish
	b = append(b, 0, 0, 0, 1, 0x68, 0xce, 0x38, 0x80)                         // PPS-ish
	b = append(b, 0, 0, 0, 1, 0x65)                                           // IDR
	b = appendDeterministicFiller(b, seed+int64(index), filler)
	return b
}

func h264PFrame(seed int64, index, filler int) []byte {
	b := []byte{0, 0, 0, 1, 0x41}
	b = appendDeterministicFiller(b, seed+int64(index), filler)
	return b
}

func appendDeterministicFiller(b []byte, seed int64, n int) []byte {
	for i := 0; i < n; i++ {
		// Letters keep payload free of accidental Annex-B zero start codes.
		b = append(b, byte('A'+(int(seed>>3)+i*7)%26))
	}
	return b
}

func aacFrame(seed int64, index, frameLen int) []byte {
	if frameLen < 16 {
		frameLen = 16
	}
	h := []byte{0xff, 0xf1, 0x4c, 0x80, 0x00, 0x1f, 0xfc}
	h[3] = 0x80 | byte((frameLen>>11)&0x03)
	h[4] = byte((frameLen >> 3) & 0xff)
	h[5] = byte((frameLen&0x07)<<5) | 0x1f
	out := append([]byte(nil), h...)
	for i := 7; i < frameLen; i++ {
		out = append(out, byte('a'+(int(seed)+index+i)%26))
	}
	return out
}

// DamagePacketIndex returns the fixed packet used by the demo sync fault.
func DamagePacketIndex() int { return damagePacketIndex }

// DamageSync flips the sync byte of a deterministic packet and returns the
// absolute injected offset.
func DamageSync(ts []byte, packet int) ([]byte, int64) {
	out := append([]byte(nil), ts...)
	off := packet * ingest.PacketSize
	if off < len(out) {
		out[off] = 0x00
	}
	return out, int64(off)
}
