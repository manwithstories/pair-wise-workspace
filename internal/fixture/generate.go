// Package fixture generates the self-contained sample corpus used by the
// scanner and the tests: two DASH representations, an HLS playlist with
// two discontinuities, and twenty deterministic fMP4 segments. All bytes
// are derived from fixed seeds, so generation is reproducible and never
// references external files.
package fixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// Corpus layout constants.
const (
	Timescale      = 90000
	SamplesPerSeg  = 12
	SampleDuration = 3750 // 1/24 s at 90 kHz
	SegDuration    = SamplesPerSeg * SampleDuration
	HLSSegments    = 10
	DashSegments   = 5 // per representation
	DashRepA       = "a"
	DashRepB       = "b"
	// TotalSegments is the number of fMP4 segments in a full corpus.
	TotalSegments = HLSSegments + 2*DashSegments
)

// Fault selects a defect injected into the generated corpus.
type Fault string

const (
	FaultNone            Fault = ""
	FaultTruncatedBox    Fault = "truncated-box"
	FaultMissingSegment  Fault = "missing-segment"
	FaultInvalidDuration Fault = "invalid-duration"
	FaultTfdtRollback    Fault = "tfdt-rollback"
	FaultDiscBoundary    Fault = "discontinuity-boundary"
)

// ParseFault validates a --fault flag value.
func ParseFault(s string) (Fault, error) {
	switch Fault(s) {
	case FaultNone, FaultTruncatedBox, FaultMissingSegment,
		FaultInvalidDuration, FaultTfdtRollback, FaultDiscBoundary:
		return Fault(s), nil
	}
	return FaultNone, fmt.Errorf("unknown fault %q (want one of: truncated-box, missing-segment, invalid-duration, tfdt-rollback, discontinuity-boundary)", s)
}

func concat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// box returns a size-prefixed MP4 box.
func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	copy(b[4:], typ)
	copy(b[8:], payload)
	return b
}

// fullBox prepends the version/flags word to a box payload.
func fullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	head := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return box(typ, append(head, payload...))
}

// sampleBytes builds one deterministic AVCC sample: a 4-byte length
// prefix followed by a single NAL unit whose payload is derived from a
// SHA-256 chain over the seed.
func sampleBytes(seed string, sampleIdx int, idr bool) []byte {
	nalHeader := byte(0x65) // AVC IDR slice (nal_unit_type 5)
	if !idr {
		nalHeader = 0x41 // non-IDR slice (nal_unit_type 1)
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/sample-%d", seed, sampleIdx)))
	payloadLen := 24 + (sampleIdx%3)*8
	payload := []byte{nalHeader}
	for len(payload) < payloadLen {
		payload = append(payload, h[:]...)
		h = sha256.Sum256(h[:])
	}
	payload = payload[:payloadLen]
	out := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(out, uint32(len(payload)))
	copy(out[4:], payload)
	return out
}

// segParams describes one generated media segment.
type segParams struct {
	seq  uint32 // mfhd sequence number
	tfdt uint64 // baseMediaDecodeTime in Timescale units
	seed string
}

// buildSegment renders one fMP4 segment: styp, moof (mfhd+traf), mdat.
// Every sixth sample is an IDR, so each segment carries two keyframes.
func buildSegment(p segParams) []byte {
	samples := make([][]byte, SamplesPerSeg)
	for i := range samples {
		samples[i] = sampleBytes(p.seed, i, i%6 == 0)
	}

	mfhdPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(mfhdPayload, p.seq)
	mfhd := fullBox("mfhd", 0, 0, mfhdPayload)

	tfhdPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(tfhdPayload, 1) // track_ID
	tfhd := fullBox("tfhd", 0, 0x020000, tfhdPayload) // default-base-is-moof

	tfdtPayload := make([]byte, 8)
	binary.BigEndian.PutUint64(tfdtPayload, p.tfdt)
	tfdt := fullBox("tfdt", 1, 0, tfdtPayload)

	// trun: data-offset + per-sample duration/size/flags.
	trunBody := make([]byte, 0, 8+SamplesPerSeg*12)
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], uint32(SamplesPerSeg))
	trunBody = append(trunBody, tmp[:]...)
	trunBody = append(trunBody, 0, 0, 0, 0) // data-offset, patched below
	for i, s := range samples {
		flags := uint32(0x00010000) // sample_is_non_sync_sample
		if i%6 == 0 {
			flags = 0x02000000 // depends_on=2 (intra), sync bit clear
		}
		binary.BigEndian.PutUint32(tmp[:], SampleDuration)
		trunBody = append(trunBody, tmp[:]...)
		binary.BigEndian.PutUint32(tmp[:], uint32(len(s)))
		trunBody = append(trunBody, tmp[:]...)
		binary.BigEndian.PutUint32(tmp[:], flags)
		trunBody = append(trunBody, tmp[:]...)
	}
	trun := fullBox("trun", 0, 0x000701, trunBody)

	traf := box("traf", concat(tfhd, tfdt, trun))
	moof := box("moof", concat(mfhd, traf))

	// Patch the trun data-offset: moof size + mdat header size, relative
	// to the start of moof (default-base-is-moof).
	dataOffsetField := 8 + len(mfhd) + 8 + len(tfhd) + len(tfdt) + 12 + 4
	binary.BigEndian.PutUint32(moof[dataOffsetField:], uint32(len(moof)+8))

	var mdatPayload []byte
	for _, s := range samples {
		mdatPayload = append(mdatPayload, s...)
	}
	mdat := box("mdat", mdatPayload)

	styp := box("styp", []byte("msdh\x00\x00\x00\x00msdhmsix"))
	return concat(styp, moof, mdat)
}
