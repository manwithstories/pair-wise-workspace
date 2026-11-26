// Package fixture deterministically generates CMAF/fMP4 fragment fixtures
// covering every fault class the pipeline must handle: mfhd gaps, tfdt
// rollback, duplicate/conflicting fragments, truncated trun/mdat, graph
// cycles and resumable slow-consumer runs. All bytes are synthesized; no
// media is ever decoded or fetched from the network.
package fixture

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// box wraps a payload with an 8-byte ISO BMFF small-size header.
func box(boxType string, payload []byte) []byte {
	if len(boxType) != 4 {
		panic("box type must be 4 bytes")
	}
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[:4], uint32(8+len(payload)))
	copy(out[4:8], boxType)
	copy(out[8:], payload)
	return out
}

// fullBox prefixes a version/flags word to a payload.
func fullBox(version uint8, flags uint32, body []byte) []byte {
	out := make([]byte, 4+len(body))
	out[0] = version
	binary.BigEndian.PutUint32(out, flags) // top byte overwritten by version
	out[0] = version
	copy(out[4:], body)
	return out
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func u64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func i32(v int32) []byte { return u32(uint32(v)) }

// SampleSpec is one synthetic sample.
type SampleSpec struct {
	Duration          uint32
	Payload           []byte
	CompositionOffset int32
	Sync              bool
}

// FragmentSpec fully describes one moof+mdat fragment to synthesize.
type FragmentSpec struct {
	TrackID  uint32
	Sequence uint32
	Tfdt     uint64
	Samples  []SampleSpec
}

const (
	trunFlags = 0x000f01 // data-offset + duration,size,flags,cto per sample
	tfhdFlags = 0x020000 // default-base-is-moof
)

// BuildFragment serializes one fragment. Sample payloads are produced from a
// deterministic content key so identical specs yield identical bytes.
func BuildFragment(spec FragmentSpec) []byte {
	mfhd := box("mfhd", fullBox(0, 0, u32(spec.Sequence)))
	tfhd := box("tfhd", fullBox(0, tfhdFlags, u32(spec.TrackID)))
	tfdt := box("tfdt", fullBox(1, 0, u64(spec.Tfdt)))

	// Assemble mdat payload and trun entries first so data_offset is known.
	var mdatPayload []byte
	entries := make([][]byte, 0, len(spec.Samples))
	for _, s := range spec.Samples {
		var flags uint32
		if !s.Sync {
			flags = 0x00010000 // sample_depends_on_others
		}
		entries = append(entries, concat(
			u32(s.Duration), u32(uint32(len(s.Payload))), u32(flags),
			i32(s.CompositionOffset),
		))
		mdatPayload = append(mdatPayload, s.Payload...)
	}
	mdat := box("mdat", mdatPayload)

	trunBody := u32(uint32(len(spec.Samples)))
	// data_offset placeholder patched after moof sizing (see below).
	trunBody = append(trunBody, make([]byte, 4)...)
	for _, e := range entries {
		trunBody = append(trunBody, e...)
	}
	// trun version 1: composition_time_offset is signed, matching the
	// int32 SampleCompositionTimeOffsetV1 field used by the parser.
	trun := box("trun", fullBox(1, trunFlags, trunBody))
	// moof = 8-byte header + traf(8-byte header + mfhd/tfhd/tfdt/trun); the
	// first sample begins right after the 8-byte mdat header.
	moofLen := 8 + 8 + len(mfhd) + len(tfhd) + len(tfdt) + len(trun)
	dataOffset := int32(moofLen + 8)
	// Patch inside the trun box before it gets copied into traf/moof:
	// trun header(8)+fullbox(4)+sample_count(4) precede data_offset.
	binary.BigEndian.PutUint32(trun[8+4+4:], uint32(dataOffset))
	traf := box("traf", concat(mfhd, tfhd, tfdt, trun))
	moof := box("moof", traf)

	return concat(moof, mdat)
}

func concat(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// Hash returns the lowercase hex SHA-256 of a fragment's raw bytes.
func Hash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// SamplePayload returns deterministic sample content of n bytes.
func SamplePayload(trackID, seq, sampleIndex uint32, n int) []byte {
	b := make([]byte, n)
	seed := sha256.Sum256(concat(u32(trackID), u32(seq), u32(sampleIndex)))
	for i := range b {
		b[i] = seed[i%len(seed)] ^ byte(i)
	}
	return b
}
