package fixture

import (
	"bytes"
	"encoding/binary"
	"fmt"

	bmp4 "github.com/abema/go-mp4"
)

// Track timescales used by all generated fixtures.
const (
	VideoTimescale = 90000
	AudioTimescale = 48000
	VideoTrackID   = 1
	AudioTrackID   = 2
	// AudioPayloadRate is the PCM frame rate of audio payloads
	// (16-bit stereo PCM); the track timescale stays 48000.
	AudioPayloadRate = 16000
	AudioChannels    = 2
)

// fragParams describes one generated fragment.
type fragParams struct {
	trackID     uint32
	seq         uint32
	tfdt        uint64
	tfdtVersion int
	sampleDur   uint32 // per-sample duration in timescale units
	sampleCount int
	payload     []byte
}

// marshalLeaf serializes one leaf box via go-mp4.
func marshalLeaf(box bmp4.IImmutableBox) []byte {
	var buf bytes.Buffer
	if _, err := bmp4.Marshal(&buf, box, bmp4.Context{}); err != nil {
		panic(fmt.Sprintf("fixture: marshal %v: %v", box.GetType(), err))
	}
	return buf.Bytes()
}

// container assembles a container box from already-marshalled children.
func container(typ string, children ...[]byte) []byte {
	var body []byte
	for _, c := range children {
		body = append(body, c...)
	}
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	copy(out[4:], typ)
	return append(out, body...)
}

// buildFragment assembles styp+moof(mfhd,traf(tfhd,tfdt,trun))+mdat.
func buildFragment(p fragParams) []byte {
	styp := marshalLeaf(&bmp4.Styp{
		MajorBrand:   [4]byte{'c', 'm', 'f', 'c'},
		MinorVersion: 0,
		CompatibleBrands: []bmp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'c', 'm', 'f', 'c'}},
		},
	})
	mfhd := marshalLeaf(&bmp4.Mfhd{
		FullBox:        bmp4.FullBox{Version: 0, Flags: [3]byte{0, 0, 0}},
		SequenceNumber: p.seq,
	})
	tfhd := &bmp4.Tfhd{TrackID: p.trackID}
	tfhd.SetFlags(0) // no optional fields
	tfhdBox := marshalLeaf(tfhd)
	tfdt := &bmp4.Tfdt{}
	if p.tfdtVersion == 1 {
		tfdt.Version = 1
		tfdt.BaseMediaDecodeTimeV1 = p.tfdt
	} else {
		tfdt.Version = 0
		tfdt.BaseMediaDecodeTimeV0 = uint32(p.tfdt)
	}
	tfdtBox := marshalLeaf(tfdt)
	trun := &bmp4.Trun{SampleCount: uint32(p.sampleCount)}
	trun.SetFlags(0x100 | 0x200) // sample-duration-present | sample-size-present
	per := uint32(0)
	if p.sampleCount > 0 {
		per = uint32(len(p.payload)) / uint32(p.sampleCount)
	}
	for i := 0; i < p.sampleCount; i++ {
		trun.Entries = append(trun.Entries, bmp4.TrunEntry{
			SampleDuration: p.sampleDur,
			SampleSize:     per,
		})
	}
	trunBox := marshalLeaf(trun)
	traf := container("traf", tfhdBox, tfdtBox, trunBox)
	moof := container("moof", mfhd, traf)
	mdat := marshalLeaf(&bmp4.Mdat{Data: p.payload})
	out := append(styp, moof...)
	return append(out, mdat...)
}

// audioPCM renders one audio fragment payload: 16-bit stereo PCM at
// AudioPayloadRate. silent=[start,end) is an optional zeroed interval
// in frames; fullSilence zeroes the whole fragment.
func audioPCM(frames int, phase float64, silent [2]int, fullSilence bool) ([]byte, float64) {
	buf := make([]byte, frames*AudioChannels*2)
	const amp = 12000.0
	for i := 0; i < frames; i++ {
		v := int16(0)
		inSilent := fullSilence || (i >= silent[0] && i < silent[1])
		if !inSilent {
			// 440 Hz sine, phase-continuous across fragments.
			v = int16(amp * sinLUT(2*3.141592653589793*440*float64(i)/AudioPayloadRate+phase))
		}
		binary.LittleEndian.PutUint16(buf[i*4:], uint16(v))   // left
		binary.LittleEndian.PutUint16(buf[i*4+2:], uint16(v)) // right
	}
	endPhase := phase + 2*3.141592653589793*440*float64(frames)/AudioPayloadRate
	return buf, endPhase
}

// sinLUT is math.sin wrapped in a helper so the generator stays
// deterministic across platforms (no external tables involved).
func sinLUT(x float64) float64 {
	// Reduce to [0, 2pi) then use a 7th-order Taylor approximation
	// around the nearest quadrant. Deterministic and accurate to ~1e-7.
	const twoPi = 2 * 3.141592653589793
	x = x - twoPi*float64(int64(x/twoPi))
	if x < 0 {
		x += twoPi
	}
	sign := 1.0
	if x >= 3.141592653589793 {
		x -= 3.141592653589793
		sign = -1
	}
	if x > 3.141592653589793/2 {
		x = 3.141592653589793 - x
	}
	x2 := x * x
	return sign * x * (1 + x2*(-1.0/6+x2*(1.0/120+x2*(-1.0/5040))))
}

// videoPayload renders deterministic pseudo-random video bytes for a
// fragment. dupOf >= 0 forces the payload to equal that fragment's.
func videoPayload(seq, size int, dupOf int) []byte {
	buf := make([]byte, size)
	src := seq
	if dupOf >= 0 {
		src = dupOf
	}
	// xorshift32 seeded from the source fragment: identical source
	// fragments always yield identical payloads.
	s := uint32(src*2654435761 + 1)
	for i := range buf {
		s ^= s << 13
		s ^= s >> 17
		s ^= s << 5
		buf[i] = byte(s)
	}
	return buf
}
