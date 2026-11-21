package fixture

import (
	"encoding/binary"
	"math"

	bmp4 "github.com/abema/go-mp4"
)

// corruptFragments builds the four damaged-fragment cases required by
// the acceptance flow: box length out of bounds, missing mfhd,
// duplicate mfhd and tfdt overflow. Each is a valid RTP payload; the
// damage is inside the ISO BMFF structure or the timing fields.
func corruptFragments(opt Options) []frag {
	mkTrun := func(dur uint32) []byte {
		trun := &bmp4.Trun{SampleCount: 1}
		trun.SetFlags(0x100 | 0x200)
		trun.Entries = append(trun.Entries, bmp4.TrunEntry{
			SampleDuration: dur, SampleSize: 64,
		})
		return marshalLeaf(trun)
	}
	mkTfhd := func() []byte {
		tfhd := &bmp4.Tfhd{TrackID: VideoTrackID}
		tfhd.SetFlags(0)
		return marshalLeaf(tfhd)
	}
	mkMfhd := func(seq uint32) []byte {
		return marshalLeaf(&bmp4.Mfhd{SequenceNumber: seq})
	}
	mkTfdt := func(ver int, base uint64) []byte {
		tfdt := &bmp4.Tfdt{}
		if ver == 1 {
			tfdt.Version = 1
			tfdt.BaseMediaDecodeTimeV1 = base
		} else {
			tfdt.Version = 0
			tfdt.BaseMediaDecodeTimeV0 = uint32(base)
		}
		return marshalLeaf(tfdt)
	}
	payload := make([]byte, 64)

	// Case 1: mdat declares a length far beyond the buffer.
	overflow := container("styp")
	overflow = append(overflow, container("moof",
		mkMfhd(101), container("traf", mkTfhd(), mkTfdt(1, 0), mkTrun(3000)))...)
	badMdat := make([]byte, 8)
	binary.BigEndian.PutUint32(badMdat, math.MaxUint32-7)
	copy(badMdat[4:], "mdat")
	overflow = append(overflow, badMdat...)
	overflow = append(overflow, payload...)

	// Case 2: moof without mfhd.
	noMfhd := container("styp")
	noMfhd = append(noMfhd, container("moof",
		container("traf", mkTfhd(), mkTfdt(1, 0), mkTrun(3000)))...)
	noMfhd = append(noMfhd, marshalLeaf(&bmp4.Mdat{Data: payload})...)

	// Case 3: two mfhd boxes in one moof.
	dupMfhd := container("styp")
	dupMfhd = append(dupMfhd, container("moof",
		mkMfhd(103), mkMfhd(103),
		container("traf", mkTfhd(), mkTfdt(1, 0), mkTrun(3000)))...)
	dupMfhd = append(dupMfhd, marshalLeaf(&bmp4.Mdat{Data: payload})...)

	// Case 4: 32-bit tfdt near wraparound; duration pushes it over.
	tfdtOverflow := container("styp")
	tfdtOverflow = append(tfdtOverflow, container("moof",
		mkMfhd(104),
		container("traf", mkTfhd(), mkTfdt(0, math.MaxUint32-500), mkTrun(3000)))...)
	tfdtOverflow = append(tfdtOverflow, marshalLeaf(&bmp4.Mdat{Data: payload})...)

	return []frag{
		{track: "video", trackID: VideoTrackID, seq: 101, data: overflow},
		{track: "video", trackID: VideoTrackID, seq: 102, data: noMfhd},
		{track: "video", trackID: VideoTrackID, seq: 103, data: dupMfhd},
		{track: "video", trackID: VideoTrackID, seq: 104, data: tfdtOverflow},
	}
}
