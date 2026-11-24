package mp4

import (
	"encoding/binary"
	"fmt"
)

// appendBox appends a size+type box containing payload to dst.
func appendBox(dst []byte, typ string, payload []byte) []byte {
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(8+len(payload)))
	copy(hdr[4:8], typ)
	dst = append(dst, hdr[:]...)
	dst = append(dst, payload...)
	return dst
}

// fullBox wraps body with version/flags.
func fullBox(version byte, flags uint32, body []byte) []byte {
	head := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return append(head, body...)
}

func u32Body(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func u64Body(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// EncodeFragment deterministically serializes the canonical moof/mdat dialect.
// The trun data_offset is computed from the layout and always points exactly at
// the mdat payload. Encoding the result of ParseFragment yields identical bytes
// for fragments produced by this package's generator.
func EncodeFragment(f *Fragment) ([]byte, error) {
	if f == nil {
		return nil, fmt.Errorf("mp4: nil fragment")
	}
	for i, s := range f.Samples {
		if s.Size > 0 && uint64(s.Size) > uint64(len(f.Data)) {
			return nil, fmt.Errorf("mp4: sample %d size %d exceeds mdat %d", i, s.Size, len(f.Data))
		}
	}

	mfhdBox := appendBox(nil, BoxMfhd, fullBox(0, 0, u32Body(f.Sequence)))
	tfhdBox := appendBox(nil, BoxTfhd, fullBox(0, TFHDDefaultBaseIsMoof, u32Body(f.TrackID)))

	tfdtVer := f.TFDTVersion
	var tfdtBody []byte
	if tfdtVer == 1 || f.BaseDecode > uint64(^uint32(0)) {
		tfdtVer = 1
		tfdtBody = u64Body(f.BaseDecode)
	} else {
		tfdtVer = 0
		tfdtBody = u32Body(uint32(f.BaseDecode))
	}
	tfdtBox := appendBox(nil, BoxTfdt, fullBox(tfdtVer, 0, tfdtBody))

	trunVersion := f.TrunVersion
	for _, s := range f.Samples {
		if s.CTOffset < 0 {
			trunVersion = 1
			break
		}
	}
	const flags = FlagDataOffsetPresent | FlagSampleDuration | FlagSampleSize | FlagSampleFlags | FlagSampleCTS
	trunBody := make([]byte, 0, 8+len(f.Samples)*16)
	trunBody = append(trunBody, u32Body(uint32(len(f.Samples)))...)
	trunBody = append(trunBody, 0, 0, 0, 0) // data_offset placeholder at body offset 4
	for _, s := range f.Samples {
		trunBody = append(trunBody, u32Body(s.Duration)...)
		trunBody = append(trunBody, u32Body(s.Size)...)
		trunBody = append(trunBody, u32Body(s.Flags)...)
		cts := make([]byte, 4)
		if trunVersion == 1 {
			binary.BigEndian.PutUint32(cts, uint32(s.CTOffset))
		} else {
			if s.CTOffset < 0 {
				return nil, fmt.Errorf("mp4: negative cts %d require trun v1", s.CTOffset)
			}
			binary.BigEndian.PutUint32(cts, uint32(s.CTOffset))
		}
		trunBody = append(trunBody, cts...)
	}
	trunBox := appendBox(nil, BoxTrun, fullBox(trunVersion, flags, trunBody))

	// moof(8) + mfhd + traf(8 + tfhd + tfdt + trun)
	moofLen := 8 + len(mfhdBox) + 8 + len(tfhdBox) + len(tfdtBox) + len(trunBox)
	dataOffset := uint32(moofLen + 8) // followed by mdat 8-byte header
	// data_offset field sits at trun box offset: header 8 + version/flags 4 + count 4
	binary.BigEndian.PutUint32(trunBox[16:20], dataOffset)

	trafPayload := make([]byte, 0, len(tfhdBox)+len(tfdtBox)+len(trunBox))
	trafPayload = append(trafPayload, tfhdBox...)
	trafPayload = append(trafPayload, tfdtBox...)
	trafPayload = append(trafPayload, trunBox...)
	trafBox := appendBox(nil, BoxTraf, trafPayload)

	moofPayload := make([]byte, 0, len(mfhdBox)+len(trafBox))
	moofPayload = append(moofPayload, mfhdBox...)
	moofPayload = append(moofPayload, trafBox...)
	moofBox := appendBox(nil, BoxMoof, moofPayload)

	mdatBox := appendBox(nil, BoxMdat, f.Data)
	out := make([]byte, 0, len(moofBox)+len(mdatBox))
	out = append(out, moofBox...)
	out = append(out, mdatBox...)
	return out, nil
}

// RoundTrip parses b, re-encodes it and returns both the fragment and the
// deterministic re-serialization. Callers compare the bytes for equality.
func RoundTrip(b []byte) (*Fragment, []byte, error) {
	f, err := ParseFragment(b)
	if err != nil {
		return nil, nil, err
	}
	out, err := EncodeFragment(f)
	if err != nil {
		return f, nil, err
	}
	return f, out, nil
}
