package probe

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"mediad/internal/model"
)

const (
	klvKeySize = 16
	// runInLimit is the largest partition run-in field accepted.
	runInLimit = 64
	// klvLimit caps a single non-partition KLV payload.
	klvLimit = 64 << 20
)

// MXF probes an MXF file written by mediad's fixture writer.
func MXF(path string) (model.Container, error) {
	f, err := os.Open(path)
	if err != nil {
		return model.Container{}, model.Wrap(model.KindInvalidInput, "probe.mxf", "open", err)
	}
	defer f.Close()
	return MXFReader(f)
}

func MXFReader(r io.ReadSeeker) (model.Container, error) {
	scan, err := scanMXF(r)
	if err != nil {
		return model.Container{}, err
	}
	return assembleMXF(scan)
}

func scanMXF(r io.ReadSeeker) (*mxfScan, error) {
	s := &mxfScan{
		primer:    defaultPrimer(),
		sequences: map[[32]byte]*LocalSet{},
		clips:     map[[32]byte]*LocalSet{},
		timecodes: map[[32]byte]*LocalSet{},
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var pos uint64
	hdr := make([]byte, klvKeySize)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, model.Wrap(model.KindInvalidMedia, "probe.mxf", "read KLV key", err)
		}
		var key UL
		copy(key[:], hdr)
		length, nlen, err := readBERFrom(r)
		if err != nil {
			return nil, model.Wrap(model.KindInvalidMedia, "probe.mxf", "read KLV length", err)
		}
		valOff := pos + klvKeySize + uint64(nlen)
		headerLen := klvKeySize + uint64(nlen)

		switch key {
		case ULHeaderPartition, ULBodyPartition, ULFooterPartition:
			if length > 1<<20 {
				return nil, model.New(model.KindOverLimit, "probe.mxf", "partition pack too large")
			}
			body := make([]byte, length)
			if _, err := io.ReadFull(r, body); err != nil {
				return nil, model.Wrap(model.KindInvalidMedia, "probe.mxf", "read partition", err)
			}
			if err := validatePartition(body); err != nil {
				return nil, err
			}
			pos = valOff + length
			continue
		}
		_ = headerLen

		if length > klvLimit {
			return nil, model.New(model.KindOverLimit, "probe.mxf",
				fmt.Sprintf("KLV %s payload %d exceeds 64MiB", key.String(), length))
		}
		val := make([]byte, length)
		if _, err := io.ReadFull(r, val); err != nil {
			return nil, model.Wrap(model.KindInvalidMedia, "probe.mxf", "truncated KLV value", err)
		}
		pos = valOff + length

		switch key {
		case ULPrimerPack:
			if err := s.parsePrimer(val); err != nil {
				return nil, err
			}
		case ULIndexTable:
			// Index correctness is the index subsystem's concern.
		default:
			if trackNo, ok := trackFromEssenceKey(key); ok && len(s.essence) < 64 {
				s.essence = append(s.essence, essenceSpan{trackNo: trackNo, off: valOff, length: length})
				continue
			}
			set, err := decodeLocalSet(key, valOff, length, val, s.primer)
			if err != nil {
				return nil, err
			}
			s.classify(set)
		}
	}
	if s.preface == nil {
		return nil, model.New(model.KindInvalidMedia, "probe.mxf", "no Preface set")
	}
	if s.content == nil {
		return nil, model.New(model.KindInvalidMedia, "probe.mxf", "no ContentStorage set")
	}
	return s, nil
}

func validatePartition(body []byte) error {
	if len(body) < 8 {
		return model.New(model.KindInvalidMedia, "probe.mxf", "partition pack too short")
	}
	runIn := binary.BigEndian.Uint32(body[:4])
	if runIn > runInLimit || uint64(runIn)+8 > uint64(len(body)) {
		return model.New(model.KindInvalidMedia, "probe.mxf", "invalid partition run-in")
	}
	// The partition's own local set starts after the run-in field.
	if _, err := decodeLocalSet(ULBodyPartition, 0, 0, body[4+runIn:], defaultPrimer()); err != nil {
		return err
	}
	return nil
}

func readBERFrom(r io.Reader) (uint64, int, error) {
	var one [1]byte
	if _, err := io.ReadFull(r, one[:]); err != nil {
		return 0, 0, err
	}
	if one[0] < 0x80 {
		return uint64(one[0]), 1, nil
	}
	n := int(one[0] & 0x7f)
	if n == 0 || n > 8 {
		return 0, 0, fmt.Errorf("invalid BER length prefix 0x%02x", one[0])
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, 0, err
	}
	var v uint64
	for _, c := range buf {
		v = v<<8 | uint64(c)
	}
	return v, 1 + n, nil
}

func (s *mxfScan) parsePrimer(val []byte) error {
	if len(val) < 4 {
		return model.New(model.KindInvalidMedia, "probe.mxf", "primer pack too short")
	}
	count := int(binary.BigEndian.Uint32(val))
	if count < 0 || 4+count*18 != len(val) {
		return model.New(model.KindInvalidMedia, "probe.mxf",
			fmt.Sprintf("primer declares %d entries but payload is %d bytes", count, len(val)))
	}
	p := make(primer, count)
	for i := 0; i < count; i++ {
		e := val[4+i*18:]
		tag := binary.BigEndian.Uint16(e)
		prop, ok := lookupTag(tag)
		if !ok {
			return model.New(model.KindInvalidMedia, "probe.mxf",
				fmt.Sprintf("primer tag 0x%04x is not in the profile", tag))
		}
		var ul UL
		copy(ul[:], e[2:18])
		if !prop.UL.RoundTrips(ul) {
			return model.New(model.KindInvalidMedia, "probe.mxf",
				fmt.Sprintf("primer tag 0x%04x maps to %s, expected %s (tag collision)", tag, ul.String(), prop.UL.String()))
		}
		p[tag] = prop
	}
	s.primer = p
	return nil
}

func (s *mxfScan) classify(set *LocalSet) {
	switch set.Key {
	case ULPreface:
		s.preface = set
	case ULContentStorage:
		s.content = set
	case ULSourcePackage:
		s.packages = append(s.packages, set)
	case ULTimelineTrack:
		s.tracks = append(s.tracks, set)
	case ULSequence:
		if id, ok := set.UMID(); ok {
			s.sequences[id] = set
		}
	case ULSourceClip:
		if id, ok := set.UMID(); ok {
			s.clips[id] = set
		}
	case ULTimecodeComponent:
		if id, ok := set.UMID(); ok {
			s.timecodes[id] = set
		}
	case ULCDCIDescriptor, ULWaveDescriptor:
		s.descs = append(s.descs, set)
	}
}

func (u UL) RoundTrips(other UL) bool {
	if u == other {
		return true
	}
	// Compare the identifying first 8 bytes and the registry-designator tail.
	for i := 0; i < 8; i++ {
		if u[i] != other[i] {
			return false
		}
	}
	for i := 12; i < 16; i++ {
		if u[i] != other[i] {
			return false
		}
	}
	return true
}
