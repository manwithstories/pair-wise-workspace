// Package bmff parses fragmented MP4 (fMP4) segments and indexes their
// timestamps and keyframes. Writing is implemented locally with a small
// deterministic box encoder so generated fixtures never depend on external
// files; reading uses github.com/abema/go-mp4.
package bmff

import (
	"encoding/binary"

	"avlab/internal/media"
)

// SampleDesc describes one sample inside a trun box.
type SampleDesc struct {
	Size     uint32
	Duration uint32
	// Keyframe marks an independently decodable (sync) sample.
	Keyframe bool
	// Payload is the synthetic sample content.
	Payload []byte
}

// FragmentSpec fully describes one fMP4 segment for the writer.
type FragmentSpec struct {
	Sequence uint32
	TrackID  uint32
	BaseTime media.MediaTime
	Samples  []SampleDesc
}

// fullBox writes a version/flags header followed by payload.
func fullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	head := make([]byte, 4)
	head[0] = version
	head[1] = byte(flags >> 16)
	head[2] = byte(flags >> 8)
	head[3] = byte(flags)
	// box() adds its own 8-byte header; the version/flags header is payload.
	return box(typ, append(head, payload...))
}

// box writes an ISO box with the given type and payload.
func box(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
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

// styp builds the segment type box.
func styp() []byte {
	p := append([]byte("msdh"), u32(0)...)
	p = append(p, []byte("msdh")...)
	p = append(p, []byte("msix")...)
	return box("styp", p)
}

// moof builds a movie-fragment box containing mfhd/tfhd/tfdt/trun.
func moof(spec FragmentSpec) []byte {
	mfhd := fullBox("mfhd", 0, 0, u32(spec.Sequence))
	tfhd := fullBox("tfhd", 0, 0x020000, u32(spec.TrackID)) // default-base-is-moof
	tfdt := fullBox("tfdt", 1, 0, u64(uint64(spec.BaseTime)))
	var trunPayload []byte
	trunPayload = append(trunPayload, u32(uint32(len(spec.Samples)))...)
	trunPayload = append(trunPayload, u32(0)...) // data offset, patched below
	// flags: data-offset(0x1) | sample-duration(0x100) |
	// sample-size(0x200) | sample-flags(0x400) = 0x000701
	for _, s := range spec.Samples {
		trunPayload = append(trunPayload, u32(s.Duration)...)
		trunPayload = append(trunPayload, u32(s.Size)...)
		flags := uint32(0x01010000) // sample_depends_on=1 (does not depend)
		if !s.Keyframe {
			flags = 0x02000000 // non-sync sample
		}
		trunPayload = append(trunPayload, u32(flags)...)
	}
	trun := fullBox("trun", 0, 0x000701, trunPayload)
	body := append(mfhd, tfhd...)
	body = append(body, tfdt...)
	body = append(body, trun...)
	moofBox := box("moof", body)
	// patch trun data offset: from moof start to first mdat payload byte
	offset := uint32(len(moofBox)) + 8
	patchTrunDataOffset(moofBox, offset)
	return moofBox
}

// patchTrunDataOffset walks the moof to locate trun and writes the offset.
func patchTrunDataOffset(moof []byte, offset uint32) {
	pos := 8 // skip moof header
	for pos+8 <= len(moof) {
		size := int(binary.BigEndian.Uint32(moof[pos:]))
		if string(moof[pos+4:pos+8]) == "trun" {
			// trun: 8 box header + 4 fullbox header + 4 sample count
			binary.BigEndian.PutUint32(moof[pos+16:], offset)
			return
		}
		pos += size
	}
}

func mdat(payload []byte) []byte {
	return box("mdat", payload)
}

// BuildFragment serializes one fMP4 segment deterministically.
func BuildFragment(spec FragmentSpec) ([]byte, error) {
	if len(spec.Samples) == 0 {
		return nil, media.NewFault(media.CodeInvalidArgument, false,
			"fragment must contain at least one sample")
	}
	out := styp()
	out = append(out, moof(spec)...)
	var payload []byte
	for _, s := range spec.Samples {
		if int(s.Size) != len(s.Payload) {
			return nil, media.NewFault(media.CodeInvalidArgument, false,
				"sample size does not match payload length")
		}
		payload = append(payload, s.Payload...)
	}
	out = append(out, mdat(payload)...)
	if len(out) > media.MaxSegmentBytes {
		return nil, media.NewFault(media.CodeSegmentTooLarge, false,
			"synthetic fragment exceeds 8 MiB hard limit")
	}
	return out, nil
}
