package manifest

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sort"
)

var (
	ErrTruncated          = errors.New("codec.truncated")
	ErrBadMagic           = errors.New("codec.bad_magic")
	ErrUnsupportedVersion = errors.New("codec.unsupported_version")
	ErrLengthTooLarge     = errors.New("codec.length_too_large")
	ErrCRC                = errors.New("codec.crc_mismatch")
	ErrBadRecord          = errors.New("codec.bad_record")
	ErrRecordLimit        = errors.New("codec.record_limit")
	ErrInvalidManifest    = errors.New("codec.invalid_manifest")
)

const (
	maxRecordSize = 16 << 20
	maxRecords    = 2_000_000
)

var magic = [4]byte{'S', 'M', 'F', '!'}

const (
	recordMeta   byte = 1
	recordRep    byte = 2
	recordDisc   byte = 3
	recordResync byte = 4
	recordStats  byte = 5
)

func IsDecodeError(err error) bool {
	for _, target := range []error{ErrTruncated, ErrBadMagic, ErrUnsupportedVersion, ErrLengthTooLarge, ErrCRC, ErrBadRecord, ErrRecordLimit, ErrInvalidManifest} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func Encode(m *Manifest, w io.Writer) error {
	if err := m.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	if _, err := w.Write(magic[:]); err != nil {
		return err
	}
	if _, err := w.Write([]byte{CodecVersion}); err != nil {
		return err
	}
	if err := writeRecord(w, recordMeta, encodeMeta(m)); err != nil {
		return err
	}
	for _, rep := range m.Representations {
		if err := writeRecord(w, recordRep, encodeRep(rep)); err != nil {
			return err
		}
	}
	disc := append([]Discontinuity(nil), m.Discontinuities...)
	sort.Slice(disc, func(i, j int) bool {
		if disc[i].RepresentationID != disc[j].RepresentationID {
			return disc[i].RepresentationID < disc[j].RepresentationID
		}
		return disc[i].SegmentNumber < disc[j].SegmentNumber
	})
	for _, d := range disc {
		if err := writeRecord(w, recordDisc, encodeDisc(d)); err != nil {
			return err
		}
	}
	resync := append([]ResyncPoint(nil), m.ResyncPoints...)
	sort.Slice(resync, func(i, j int) bool {
		if resync[i].RepresentationID != resync[j].RepresentationID {
			return resync[i].RepresentationID < resync[j].RepresentationID
		}
		return resync[i].SegmentNumber < resync[j].SegmentNumber
	})
	for _, r := range resync {
		if err := writeRecord(w, recordResync, encodeResync(r)); err != nil {
			return err
		}
	}
	stats := append([]RepresentationStats(nil), m.Stats...)
	sort.Slice(stats, func(i, j int) bool { return stats[i].RepresentationID < stats[j].RepresentationID })
	for _, s := range stats {
		if err := writeRecord(w, recordStats, encodeStats(s)); err != nil {
			return err
		}
	}
	return nil
}

func Decode(r io.Reader) (*Manifest, error) {
	br := bufio.NewReader(r)
	header := make([]byte, 5)
	if _, err := io.ReadFull(br, header); err != nil {
		return nil, wrapDecode(ErrTruncated, "short header", err)
	}
	if !bytes.Equal(header[:4], magic[:]) {
		return nil, fmt.Errorf("%w: got %q", ErrBadMagic, header[:4])
	}
	if header[4] != CodecVersion {
		return nil, fmt.Errorf("%w: got %d want %d", ErrUnsupportedVersion, header[4], CodecVersion)
	}
	m := &Manifest{Template: DefaultTemplate}
	metaSeen := false
	for count := 0; ; count++ {
		if count >= maxRecords {
			return nil, ErrRecordLimit
		}
		typ, payload, err := readRecord(br)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch typ {
		case recordMeta:
			if metaSeen {
				return nil, fmt.Errorf("%w: duplicate meta record", ErrBadRecord)
			}
			if err := decodeMeta(m, payload); err != nil {
				return nil, err
			}
			metaSeen = true
		case recordRep:
			rep, err := decodeRep(payload)
			if err != nil {
				return nil, err
			}
			m.Representations = append(m.Representations, rep)
		case recordDisc:
			d, err := decodeDisc(payload)
			if err != nil {
				return nil, err
			}
			m.Discontinuities = append(m.Discontinuities, d)
		case recordResync:
			p, err := decodeResync(payload)
			if err != nil {
				return nil, err
			}
			m.ResyncPoints = append(m.ResyncPoints, p)
		case recordStats:
			s, err := decodeStats(payload)
			if err != nil {
				return nil, err
			}
			m.Stats = append(m.Stats, s)
		default:
			return nil, fmt.Errorf("%w: unknown record type %d", ErrBadRecord, typ)
		}
	}
	if !metaSeen {
		return nil, fmt.Errorf("%w: missing meta record", ErrBadRecord)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	return m, nil
}

func writeRecord(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > maxRecordSize {
		return ErrLengthTooLarge
	}
	header := make([]byte, 0, 6)
	header = binary.AppendUvarint(header, uint64(len(payload)))
	header = append(header, typ)
	header = binary.BigEndian.AppendUint32(header, crc32.ChecksumIEEE(payload))
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readRecord(br *bufio.Reader) (byte, []byte, error) {
	length, err := binary.ReadUvarint(br)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return 0, nil, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, wrapDecode(ErrTruncated, "record length", err)
		}
		return 0, nil, fmt.Errorf("%w: invalid varint: %v", ErrLengthTooLarge, err)
	}
	if length > maxRecordSize {
		return 0, nil, fmt.Errorf("%w: %d", ErrLengthTooLarge, length)
	}
	typ, err := br.ReadByte()
	if err != nil {
		return 0, nil, wrapDecode(ErrTruncated, "record type", err)
	}
	var crcBytes [4]byte
	if _, err := io.ReadFull(br, crcBytes[:]); err != nil {
		return 0, nil, wrapDecode(ErrTruncated, "record crc", err)
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(br, payload); err != nil {
		return 0, nil, wrapDecode(ErrTruncated, "record payload", err)
	}
	if got, want := crc32.ChecksumIEEE(payload), binary.BigEndian.Uint32(crcBytes[:]); got != want {
		return 0, nil, fmt.Errorf("%w: got %08x want %08x", ErrCRC, got, want)
	}
	return typ, payload, nil
}

func wrapDecode(base error, context string, err error) error {
	if err == nil {
		return base
	}
	return fmt.Errorf("%w: %s: %v", base, context, err)
}

type payloadEncoder []byte

func (e *payloadEncoder) uvarint(v uint64) { *e = binary.AppendUvarint(*e, v) }
func (e *payloadEncoder) varint(v int64)   { *e = binary.AppendVarint(*e, v) }
func (e *payloadEncoder) str(v string)     { e.uvarint(uint64(len(v))); *e = append(*e, v...) }
func (e *payloadEncoder) byte(v byte)      { *e = append(*e, v) }

func encodeMeta(m *Manifest) []byte {
	e := make(payloadEncoder, 0, 64)
	e.varint(m.TargetUS)
	e.varint(m.ToleranceUS)
	e.str(m.Template)
	e.uvarint(m.Window.Version)
	e.uvarint(m.Window.StartNumber)
	e.uvarint(uint64(m.Window.Size))
	return e
}

func encodeRep(r Representation) []byte {
	e := make(payloadEncoder, 0, 64+len(r.Segments)*20)
	e.str(r.ID)
	e.uvarint(r.Bandwidth)
	e.varint(r.AudioOffsetUS)
	e.uvarint(uint64(len(r.Segments)))
	for _, s := range r.Segments {
		e.uvarint(s.Number)
		e.varint(s.StartUS)
		e.varint(s.DurationUS)
		e.varint(s.RawStartUS)
		e.varint(s.DriftUS)
		var flags byte
		if s.Truncated {
			flags |= 1
		}
		if s.Discontinuity {
			flags |= 2
		}
		e.byte(flags)
	}
	return e
}

func encodeDisc(d Discontinuity) []byte {
	e := make(payloadEncoder, 0, 80)
	e.str(d.RepresentationID)
	e.uvarint(d.SegmentNumber)
	e.varint(d.RawStartUS)
	e.varint(d.GridStartUS)
	e.varint(d.DriftUS)
	e.varint(int64(d.Direction))
	return e
}

func encodeResync(r ResyncPoint) []byte {
	e := make(payloadEncoder, 0, 64)
	e.str(r.RepresentationID)
	e.uvarint(r.SegmentNumber)
	e.varint(r.GridStartUS)
	e.varint(r.DriftUS)
	e.varint(int64(r.Direction))
	return e
}

func encodeStats(s RepresentationStats) []byte {
	e := make(payloadEncoder, 0, 80)
	e.str(s.RepresentationID)
	e.uvarint(s.Bandwidth)
	e.uvarint(uint64(s.Samples))
	e.varint(s.TotalAbsUS)
	e.varint(s.MaxDriftUS)
	e.varint(s.P95DriftUS)
	e.uvarint(uint64(s.ResyncPoints))
	return e
}

type payloadDecoder struct {
	b   []byte
	off int
}

func (d *payloadDecoder) fail(what string) error { return fmt.Errorf("%w: %s", ErrBadRecord, what) }
func (d *payloadDecoder) uvarint() (uint64, error) {
	v, n := binary.Uvarint(d.b[d.off:])
	if n <= 0 {
		return 0, d.fail("uvarint")
	}
	d.off += n
	return v, nil
}
func (d *payloadDecoder) varint() (int64, error) {
	v, n := binary.Varint(d.b[d.off:])
	if n <= 0 {
		return 0, d.fail("varint")
	}
	d.off += n
	return v, nil
}
func (d *payloadDecoder) str() (string, error) {
	n, err := d.uvarint()
	if err != nil || n > uint64(len(d.b)-d.off) {
		return "", d.fail("string length")
	}
	value := string(d.b[d.off : d.off+int(n)])
	d.off += int(n)
	return value, nil
}
func (d *payloadDecoder) byte() (byte, error) {
	if d.off >= len(d.b) {
		return 0, d.fail("byte")
	}
	v := d.b[d.off]
	d.off++
	return v, nil
}
func (d *payloadDecoder) done() error {
	if d.off != len(d.b) {
		return d.fail("trailing payload bytes")
	}
	return nil
}

func decodeMeta(m *Manifest, payload []byte) error {
	d := payloadDecoder{b: payload}
	var err error
	if m.TargetUS, err = d.varint(); err != nil {
		return err
	}
	if m.ToleranceUS, err = d.varint(); err != nil {
		return err
	}
	if m.Template, err = d.str(); err != nil {
		return err
	}
	var v uint64
	if v, err = d.uvarint(); err != nil {
		return err
	}
	m.Window.Version = v
	if v, err = d.uvarint(); err != nil {
		return err
	}
	m.Window.StartNumber = v
	if v, err = d.uvarint(); err != nil {
		return err
	}
	m.Window.Size = int(v)
	return d.done()
}

func decodeRep(payload []byte) (Representation, error) {
	d := payloadDecoder{b: payload}
	var r Representation
	var err error
	if r.ID, err = d.str(); err != nil {
		return r, err
	}
	if r.Bandwidth, err = d.uvarint(); err != nil {
		return r, err
	}
	if r.AudioOffsetUS, err = d.varint(); err != nil {
		return r, err
	}
	count, err := d.uvarint()
	if err != nil || count > maxRecords {
		return r, d.fail("segment count")
	}
	r.Segments = make([]Segment, 0, int(count))
	for i := uint64(0); i < count; i++ {
		var s Segment
		if s.Number, err = d.uvarint(); err != nil {
			return r, err
		}
		if s.StartUS, err = d.varint(); err != nil {
			return r, err
		}
		if s.DurationUS, err = d.varint(); err != nil {
			return r, err
		}
		if s.RawStartUS, err = d.varint(); err != nil {
			return r, err
		}
		if s.DriftUS, err = d.varint(); err != nil {
			return r, err
		}
		flags, err := d.byte()
		if err != nil {
			return r, err
		}
		if flags&^byte(3) != 0 {
			return r, d.fail("segment flags")
		}
		s.Truncated, s.Discontinuity = flags&1 != 0, flags&2 != 0
		r.Segments = append(r.Segments, s)
	}
	return r, d.done()
}

func decodeDisc(payload []byte) (Discontinuity, error) {
	d := payloadDecoder{b: payload}
	var v Discontinuity
	var err error
	if v.RepresentationID, err = d.str(); err != nil {
		return v, err
	}
	if v.SegmentNumber, err = d.uvarint(); err != nil {
		return v, err
	}
	if v.RawStartUS, err = d.varint(); err != nil {
		return v, err
	}
	if v.GridStartUS, err = d.varint(); err != nil {
		return v, err
	}
	if v.DriftUS, err = d.varint(); err != nil {
		return v, err
	}
	dir, err := d.varint()
	if err != nil {
		return v, err
	}
	v.Direction = int(dir)
	return v, d.done()
}

func decodeResync(payload []byte) (ResyncPoint, error) {
	d := payloadDecoder{b: payload}
	var v ResyncPoint
	var err error
	if v.RepresentationID, err = d.str(); err != nil {
		return v, err
	}
	if v.SegmentNumber, err = d.uvarint(); err != nil {
		return v, err
	}
	if v.GridStartUS, err = d.varint(); err != nil {
		return v, err
	}
	if v.DriftUS, err = d.varint(); err != nil {
		return v, err
	}
	dir, err := d.varint()
	if err != nil {
		return v, err
	}
	v.Direction = int(dir)
	return v, d.done()
}

func decodeStats(payload []byte) (RepresentationStats, error) {
	d := payloadDecoder{b: payload}
	var v RepresentationStats
	var err error
	if v.RepresentationID, err = d.str(); err != nil {
		return v, err
	}
	if v.Bandwidth, err = d.uvarint(); err != nil {
		return v, err
	}
	samples, err := d.uvarint()
	if err != nil {
		return v, err
	}
	v.Samples = int(samples)
	if v.TotalAbsUS, err = d.varint(); err != nil {
		return v, err
	}
	if v.MaxDriftUS, err = d.varint(); err != nil {
		return v, err
	}
	if v.P95DriftUS, err = d.varint(); err != nil {
		return v, err
	}
	resyncs, err := d.uvarint()
	if err != nil {
		return v, err
	}
	v.ResyncPoints = int(resyncs)
	return v, d.done()
}
