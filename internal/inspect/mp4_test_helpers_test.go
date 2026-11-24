package inspect

import (
	"bytes"
	"encoding/binary"
)

// testSample is one encoded sample for fixture fragments.
type testSample struct {
	nalu      []byte // NAL payload without length prefix
	duration  uint32
}

// avcNAL builds an AVC NAL unit (header byte + payload) without any length
// prefix; the sample builder adds the 4-byte length prefix.
func avcNAL(nalType byte, payload []byte) []byte {
	hdr := nalType & 0x1f
	return append([]byte{hdr}, payload...)
}

// box wraps a payload in an ISOBMFF box (size + fourcc + payload).
func box(fourcc string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(payload)))
	copy(out[4:8], fourcc)
	copy(out[8:], payload)
	return out
}

// fullBoxPayload prefixes version+flags (4 bytes) to a box payload.
func fullBoxPayload(flags uint32, body []byte) []byte {
	out := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(out[0:4], flags)
	copy(out[4:], body)
	return out
}

// buildFragment serializes a minimal moof+mdat fragment. trun flags carry
// sample-duration (0x000100) and sample-size (0x000200) present bits, matching
// go-mp4's TrunEntry optional field gates.
func buildFragment(trackID uint32, baseDecode uint64, samples []testSample) []byte {
	// tfhd
	tfhdBody := make([]byte, 4)
	binary.BigEndian.PutUint32(tfhdBody, trackID)
	tfhd := box("tfhd", fullBoxPayload(0x000000, tfhdBody))

	// tfdt (version 1, 64-bit base media decode time)
	tfdtBody := make([]byte, 8)
	binary.BigEndian.PutUint64(tfdtBody, baseDecode)
	tfdt := box("tfdt", fullBoxPayload(0x01000000, tfdtBody))

	// trun with duration + size
	var trunBody bytes.Buffer
	count := make([]byte, 4)
	binary.BigEndian.PutUint32(count, uint32(len(samples)))
	trunBody.Write(count)
	var mdat bytes.Buffer
	for _, s := range samples {
		dur := make([]byte, 4)
		binary.BigEndian.PutUint32(dur, s.duration)
		trunBody.Write(dur)
		size := make([]byte, 4)
		binary.BigEndian.PutUint32(size, uint32(len(s.nalu)))
		trunBody.Write(size)
		mdat.Write(s.nalu)
	}
	trun := box("trun", fullBoxPayload(0x000300, trunBody.Bytes()))

	mfhd := box("mfhd", fullBoxPayload(0, []byte{0, 0, 0, 1}))
	var moofInner bytes.Buffer
	moofInner.Write(mfhd)
	moofInner.Write(tfhd)
	moofInner.Write(tfdt)
	moofInner.Write(trun)
	moof := box("moof", moofInner.Bytes())

	mdatBox := box("mdat", mdat.Bytes())
	return append(moof, mdatBox...)
}

// sample wraps a NAL unit (header+payload) with its 4-byte length prefix.
func sample(nal []byte, duration uint32) testSample {
	prefixed := make([]byte, 4+len(nal))
	binary.BigEndian.PutUint32(prefixed, uint32(len(nal)))
	copy(prefixed[4:], nal)
	return testSample{nalu: prefixed, duration: duration}
}

// keyframeSample builds an AVC IDR sample with one length-prefixed NAL unit.
func keyframeSample(duration uint32) testSample {
	return sample(avcNAL(avcNALIDR, []byte("idr-payload")), duration)
}

// deltaSample builds a non-IDR (type 1) sample.
func deltaSample(duration uint32) testSample {
	return sample(avcNAL(1, []byte("p-frame")), duration)
}

// withZeroDuration clones samples but zeroes the first sample's duration.
func withZeroDuration(samples []testSample) []testSample {
	out := append([]testSample(nil), samples...)
	out[0].duration = 0
	return out
}
