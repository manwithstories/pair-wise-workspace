// Package meta implements lossless round-trip codecs for the timed metadata
// that shares the audio timeline: subtitles, chapters, ID3 tags, DASH emsg
// boxes and H.264 SEI NAL units.
//
// Every codec parses a payload into a structured value and re-encodes it. The
// encoder is byte-exact for well-formed input; the parser rejects oversize or
// truncated records with a located error instead of inventing media.
package meta

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"mediacore/internal/clock"
)

// Kind identifies a metadata record family.
type Kind string

const (
	KindSubtitle Kind = "subtitle"
	KindChapter  Kind = "chapter"
	KindID3      Kind = "id3"
	KindEmsg     Kind = "emsg"
	KindSEI      Kind = "sei"
)

// MaxRecordBytes bounds any single metadata record handled by the core.
const MaxRecordBytes = 1 << 20

// ErrTruncated reports a payload shorter than its declared structure.
var ErrTruncated = errors.New("meta: truncated record")

// ErrOversize reports a record or declared length beyond MaxRecordBytes.
var ErrOversize = errors.New("meta: record exceeds size limit")

// ErrMalformed reports a structurally invalid record.
var ErrMalformed = errors.New("meta: malformed record")

// Record is the common, timeline-aware envelope for every metadata kind.
// PTS and Duration are in TimeScale ticks; Unified holds the derived position
// on the shared audio timeline in microseconds.
type Record struct {
	Kind      Kind
	Track     int
	TimeScale clock.Rational
	PTS       int64
	Duration  int64
	Unified   int64
	Payload   []byte
	Text      string
	Fields    map[string]string
}

// checkLen enforces the per-record limit before any allocation.
func checkLen(kind Kind, n int) error {
	if n < 0 {
		return fmt.Errorf("%w: %s negative length %d", ErrMalformed, kind, n)
	}
	if n > MaxRecordBytes {
		return fmt.Errorf("%w: %s length %d > %d", ErrOversize, kind, n, MaxRecordBytes)
	}
	return nil
}

// ---------------------------------------------------------------------------
// ID3v2
// ---------------------------------------------------------------------------

// ID3Frame is one ID3v2 frame kept verbatim.
type ID3Frame struct {
	ID    string
	Flags uint16
	Data  []byte
}

// ID3Tag is a decoded ID3v2 tag. Version, Revision, Flags and Padding are kept
// so encoding reproduces the original tag byte for byte.
type ID3Tag struct {
	Version  byte
	Revision byte
	Flags    byte
	Frames   []ID3Frame
	Padding  int
	rawSize  int
}

func syncsafe(b []byte) (int, bool) {
	if len(b) < 4 {
		return 0, false
	}
	if b[0]&0x80 != 0 || b[1]&0x80 != 0 || b[2]&0x80 != 0 || b[3]&0x80 != 0 {
		return 0, false
	}
	return int(b[0])<<21 | int(b[1])<<14 | int(b[2])<<7 | int(b[3]), true
}

func putSyncsafe(dst []byte, n int) {
	dst[0] = byte(n >> 21 & 0x7f)
	dst[1] = byte(n >> 14 & 0x7f)
	dst[2] = byte(n >> 7 & 0x7f)
	dst[3] = byte(n & 0x7f)
}

// ParseID3 decodes an ID3v2.3 or v2.4 tag.
func ParseID3(b []byte) (*ID3Tag, error) {
	if err := checkLen(KindID3, len(b)); err != nil {
		return nil, err
	}
	if len(b) < 10 || string(b[0:3]) != "ID3" {
		return nil, fmt.Errorf("%w: missing ID3 header", ErrMalformed)
	}
	if b[3] != 3 && b[3] != 4 {
		return nil, fmt.Errorf("%w: unsupported ID3 version 2.%d", ErrMalformed, b[3])
	}
	size, ok := syncsafe(b[6:10])
	if !ok {
		return nil, fmt.Errorf("%w: non-syncsafe ID3 size", ErrMalformed)
	}
	if 10+size > len(b) {
		return nil, fmt.Errorf("%w: ID3 declares %d bytes, have %d", ErrTruncated, size, len(b)-10)
	}
	t := &ID3Tag{Version: b[3], Revision: b[4], Flags: b[5], rawSize: 10 + size}
	body := b[10 : 10+size]
	for len(body) >= 10 {
		if body[0] == 0 {
			break // padding
		}
		id := string(body[0:4])
		var fsz int
		if t.Version == 4 {
			var ok bool
			fsz, ok = syncsafe(body[4:8])
			if !ok {
				return nil, fmt.Errorf("%w: frame %q non-syncsafe size", ErrMalformed, id)
			}
		} else {
			fsz = int(binary.BigEndian.Uint32(body[4:8]))
		}
		flags := binary.BigEndian.Uint16(body[8:10])
		if fsz < 0 || 10+fsz > len(body) {
			return nil, fmt.Errorf("%w: frame %q declares %d bytes, have %d", ErrTruncated, id, fsz, len(body)-10)
		}
		t.Frames = append(t.Frames, ID3Frame{ID: id, Flags: flags, Data: append([]byte(nil), body[10:10+fsz]...)})
		body = body[10+fsz:]
	}
	t.Padding = len(body)
	return t, nil
}

// Encode reproduces the tag bytes. It returns ErrOversize for tags beyond the
// per-record limit.
func (t *ID3Tag) Encode() ([]byte, error) {
	var body bytes.Buffer
	for _, f := range t.Frames {
		if len(f.ID) != 4 {
			return nil, fmt.Errorf("%w: frame id %q", ErrMalformed, f.ID)
		}
		var sz [4]byte
		if t.Version == 4 {
			putSyncsafe(sz[:], len(f.Data))
		} else {
			binary.BigEndian.PutUint32(sz[:], uint32(len(f.Data)))
		}
		body.WriteString(f.ID)
		body.Write(sz[:])
		var fl [2]byte
		binary.BigEndian.PutUint16(fl[:], f.Flags)
		body.Write(fl[:])
		body.Write(f.Data)
	}
	body.Write(make([]byte, t.Padding))
	total := body.Len()
	if err := checkLen(KindID3, total); err != nil {
		return nil, err
	}
	out := make([]byte, 10, 10+total)
	copy(out, "ID3")
	out[3], out[4], out[5] = t.Version, t.Revision, t.Flags
	putSyncsafe(out[6:10], total)
	return append(out, body.Bytes()...), nil
}

// TextFrames returns the decoded text frames (T***) as id -> value.
func (t *ID3Tag) TextFrames() map[string]string {
	out := make(map[string]string)
	for _, f := range t.Frames {
		if len(f.ID) == 0 || f.ID[0] != 'T' || f.ID == "TXXX" || len(f.Data) < 1 {
			continue
		}
		out[f.ID], _, _ = DecodeID3Text(f.Data)
	}
	return out
}

// DecodeID3Text decodes a text frame body, returning the string and the byte
// count consumed (nil consumed signals the whole body).
func DecodeID3Text(b []byte) (string, int, error) {
	if len(b) < 1 {
		return "", 0, fmt.Errorf("%w: empty text frame", ErrMalformed)
	}
	enc := b[0]
	data := b[1:]
	switch enc {
	case 0, 3:
		data = bytes.TrimRight(data, "\x00")
		return string(data), len(b), nil
	case 1, 2:
		if len(data)%2 == 1 {
			data = data[:len(data)-1]
		}
		u := make([]uint16, len(data)/2)
		for i := range u {
			u[i] = binary.BigEndian.Uint16(data[i*2:])
		}
		rs := make([]rune, 0, len(u))
		for i := 0; i < len(u); i++ {
			if u[i] == 0 {
				break
			}
			if u[i] >= 0xD800 && u[i] < 0xDC00 && i+1 < len(u) {
				r := 0x10000 + (rune(u[i])-0xD800)<<10 + (rune(u[i+1]) - 0xDC00)
				rs = append(rs, r)
				i++
				continue
			}
			rs = append(rs, rune(u[i]))
		}
		return string(rs), len(b), nil
	}
	return "", 0, fmt.Errorf("%w: unknown ID3 text encoding %d", ErrMalformed, enc)
}

// ---------------------------------------------------------------------------
// DASH emsg
// ---------------------------------------------------------------------------

// Emsg is a decoded DASH event message box.
type Emsg struct {
	Version       byte
	Flags         uint32
	SchemeIDURI   string
	Value         string
	Timescale     uint32
	Presentation  int64 // version 1 absolute time
	PTDelta       uint32
	EventDuration uint32
	ID            uint32
	Message       []byte
	// ID3 is the parsed payload when the scheme carries an ID3 tag.
	ID3 *ID3Tag
}

// ParseEmsg decodes an emsg payload (the bytes following the box header).
func ParseEmsg(b []byte) (*Emsg, error) {
	if err := checkLen(KindEmsg, len(b)); err != nil {
		return nil, err
	}
	if len(b) < 4 {
		return nil, fmt.Errorf("%w: emsg header", ErrTruncated)
	}
	e := &Emsg{Version: b[0]}
	e.Flags = uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	p := b[4:]
	if e.Version == 0 {
		var err error
		if e.SchemeIDURI, p, err = readCString(p); err != nil {
			return nil, err
		}
		if e.Value, p, err = readCString(p); err != nil {
			return nil, err
		}
		if len(p) < 16 {
			return nil, fmt.Errorf("%w: emsg v0 timing fields", ErrTruncated)
		}
		e.Timescale = binary.BigEndian.Uint32(p[0:4])
		e.PTDelta = binary.BigEndian.Uint32(p[4:8])
		e.EventDuration = binary.BigEndian.Uint32(p[8:12])
		e.ID = binary.BigEndian.Uint32(p[12:16])
		e.Message = append([]byte(nil), p[16:]...)
	} else if e.Version == 1 {
		if len(p) < 20 {
			return nil, fmt.Errorf("%w: emsg v1 timing fields", ErrTruncated)
		}
		e.Timescale = binary.BigEndian.Uint32(p[0:4])
		e.Presentation = int64(binary.BigEndian.Uint64(p[4:12]))
		e.EventDuration = binary.BigEndian.Uint32(p[12:16])
		e.ID = binary.BigEndian.Uint32(p[16:20])
		var err error
		if e.SchemeIDURI, p, err = readCString(p[20:]); err != nil {
			return nil, err
		}
		if e.Value, p, err = readCString(p); err != nil {
			return nil, err
		}
		e.Message = append([]byte(nil), p...)
	} else {
		return nil, fmt.Errorf("%w: unsupported emsg version %d", ErrMalformed, e.Version)
	}
	if e.Timescale == 0 {
		return nil, fmt.Errorf("%w: emsg timescale 0", ErrMalformed)
	}
	if bytes.HasPrefix(e.Message, []byte("ID3")) {
		if tag, err := ParseID3(e.Message); err == nil {
			e.ID3 = tag
		}
	}
	return e, nil
}

// Encode rebuilds the emsg payload byte-exactly.
func (e *Emsg) Encode() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte(e.Version)
	b.WriteByte(byte(e.Flags >> 16))
	b.WriteByte(byte(e.Flags >> 8))
	b.WriteByte(byte(e.Flags))
	if e.Version == 0 {
		b.WriteString(e.SchemeIDURI)
		b.WriteByte(0)
		b.WriteString(e.Value)
		b.WriteByte(0)
		var buf [4]byte
		binary.BigEndian.PutUint32(buf[:], e.Timescale)
		b.Write(buf[:])
		binary.BigEndian.PutUint32(buf[:], e.PTDelta)
		b.Write(buf[:])
		binary.BigEndian.PutUint32(buf[:], e.EventDuration)
		b.Write(buf[:])
		binary.BigEndian.PutUint32(buf[:], e.ID)
		b.Write(buf[:])
		b.Write(e.Message)
	} else if e.Version == 1 {
		var buf [8]byte
		binary.BigEndian.PutUint32(buf[:4], e.Timescale)
		b.Write(buf[:4])
		binary.BigEndian.PutUint64(buf[:], uint64(e.Presentation))
		b.Write(buf[:])
		binary.BigEndian.PutUint32(buf[:4], e.EventDuration)
		b.Write(buf[:4])
		binary.BigEndian.PutUint32(buf[:4], e.ID)
		b.Write(buf[:4])
		b.WriteString(e.SchemeIDURI)
		b.WriteByte(0)
		b.WriteString(e.Value)
		b.WriteByte(0)
		b.Write(e.Message)
	} else {
		return nil, fmt.Errorf("%w: unsupported emsg version %d", ErrMalformed, e.Version)
	}
	if err := checkLen(KindEmsg, b.Len()); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// PresentationTicks returns the event time in Timescale ticks.
func (e *Emsg) PresentationTicks() int64 {
	if e.Version == 0 {
		return int64(e.PTDelta)
	}
	return e.Presentation
}

func readCString(b []byte) (string, []byte, error) {
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return "", nil, fmt.Errorf("%w: unterminated string", ErrTruncated)
	}
	if i > MaxRecordBytes {
		return "", nil, fmt.Errorf("%w: string length %d", ErrOversize, i)
	}
	return string(b[:i]), b[i+1:], nil
}

// ---------------------------------------------------------------------------
// H.264 SEI
// ---------------------------------------------------------------------------

// SEIMessage is one SEI message kept as type + raw payload.
type SEIMessage struct {
	Type    int
	Payload []byte
}

// SEI is a decoded SEI NAL unit.
type SEI struct {
	Messages []SEIMessage
	// UUID and UserData are filled for user_data_unregistered (type 5).
	UUID     []byte
	UserData []byte
}

// ParseSEINAL decodes a NAL unit whose type is SEI (6) or a raw SEI payload
// (when the NAL header has already been stripped).
func ParseSEINAL(nal []byte) (*SEI, error) {
	if err := checkLen(KindSEI, len(nal)); err != nil {
		return nil, err
	}
	if len(nal) < 1 {
		return nil, fmt.Errorf("%w: empty NAL", ErrTruncated)
	}
	p := nal
	if nal[0]&0x1f == 6 {
		p = nal[1:] // skip NAL header
	}
	s := &SEI{}
	for len(p) > 0 {
		if p[0] == 0x80 { // rbsp trailing bits
			break
		}
		typ, n, ok := readSEIValue(p)
		if !ok {
			return nil, fmt.Errorf("%w: sei type", ErrTruncated)
		}
		p = p[n:]
		size, n, ok := readSEIValue(p)
		if !ok {
			return nil, fmt.Errorf("%w: sei size", ErrTruncated)
		}
		p = p[n:]
		if size > len(p) {
			return nil, fmt.Errorf("%w: sei message declares %d, have %d", ErrTruncated, size, len(p))
		}
		if size > MaxRecordBytes {
			return nil, fmt.Errorf("%w: sei message %d", ErrOversize, size)
		}
		msg := SEIMessage{Type: typ, Payload: append([]byte(nil), p[:size]...)}
		s.Messages = append(s.Messages, msg)
		if typ == 5 && len(msg.Payload) >= 16 {
			s.UUID = append([]byte(nil), msg.Payload[:16]...)
			s.UserData = append([]byte(nil), msg.Payload[16:]...)
		}
		p = p[size:]
	}
	return s, nil
}

// Encode rebuilds the SEI payload (without the NAL header unless withHeader).
func (s *SEI) Encode(withHeader bool) ([]byte, error) {
	var b bytes.Buffer
	if withHeader {
		b.WriteByte(0x06)
	}
	for _, m := range s.Messages {
		writeSEIValue(&b, m.Type)
		writeSEIValue(&b, len(m.Payload))
		b.Write(m.Payload)
	}
	b.WriteByte(0x80)
	if err := checkLen(KindSEI, b.Len()); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func readSEIValue(b []byte) (val, n int, ok bool) {
	for n = 0; n < len(b); n++ {
		val += int(b[n])
		if b[n] != 0xff {
			return val, n + 1, true
		}
		if val > MaxRecordBytes {
			return 0, 0, false
		}
	}
	return 0, 0, false
}

func writeSEIValue(b *bytes.Buffer, v int) {
	for v >= 0xff {
		b.WriteByte(0xff)
		v -= 0xff
	}
	b.WriteByte(byte(v))
}

// ---------------------------------------------------------------------------
// Subtitles and chapters
// ---------------------------------------------------------------------------

// mov_text stores a 2-byte big-endian text length followed by UTF-8 text and
// optional trailing style boxes. SubRip/MKV subtitles are plain text.
func ParseSubtitle(codec string, b []byte) (text string, styleTail []byte, err error) {
	if err := checkLen(KindSubtitle, len(b)); err != nil {
		return "", nil, err
	}
	if codec == "mov_text" || codec == "tx3g" {
		if len(b) < 2 {
			return "", nil, fmt.Errorf("%w: mov_text length", ErrTruncated)
		}
		n := int(binary.BigEndian.Uint16(b[:2]))
		if 2+n > len(b) {
			return "", nil, fmt.Errorf("%w: mov_text declares %d bytes, have %d", ErrTruncated, n, len(b)-2)
		}
		return string(b[2 : 2+n]), append([]byte(nil), b[2+n:]...), nil
	}
	return string(bytes.TrimRight(b, "\x00")), nil, nil
}

// EncodeSubtitle rebuilds a subtitle payload exactly.
func EncodeSubtitle(codec, text string, styleTail []byte) ([]byte, error) {
	if err := checkLen(KindSubtitle, len(text)+len(styleTail)+2); err != nil {
		return nil, err
	}
	if codec == "mov_text" || codec == "tx3g" {
		if len(text) > 0xffff {
			return nil, fmt.Errorf("%w: mov_text text %d bytes", ErrOversize, len(text))
		}
		out := make([]byte, 2, 2+len(text)+len(styleTail))
		binary.BigEndian.PutUint16(out, uint16(len(text)))
		out = append(out, text...)
		return append(out, styleTail...), nil
	}
	return append([]byte(text), styleTail...), nil
}

// Chapter is a chapter record on the unified timeline.
type Chapter struct {
	ID       int64
	Title    string
	StartUs  int64
	EndUs    int64
	Metadata map[string]string
}

// EncodeFFMetadata renders chapters as FFmpeg metadata text.
func EncodeFFMetadata(chs []Chapter) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(";FFMETADATA1\n")
	for _, ch := range chs {
		if ch.EndUs < ch.StartUs || ch.StartUs < 0 {
			return nil, fmt.Errorf("%w: chapter %d [%d,%d)", clock.ErrNegativeDuration, ch.ID, ch.StartUs, ch.EndUs)
		}
		fmt.Fprintf(&b, "[CHAPTER]\nTIMEBASE=1/1000000\nSTART=%d\nEND=%d\ntitle=%s\n", ch.StartUs, ch.EndUs, escapeFF(ch.Title))
		for k, v := range ch.Metadata {
			if k == "title" {
				continue
			}
			fmt.Fprintf(&b, "%s=%s\n", k, escapeFF(v))
		}
	}
	if err := checkLen(KindChapter, b.Len()); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ParseFFMetadata decodes the chapter records written by EncodeFFMetadata.
func ParseFFMetadata(b []byte) ([]Chapter, error) {
	if err := checkLen(KindChapter, len(b)); err != nil {
		return nil, err
	}
	lines := bytes.Split(b, []byte("\n"))
	var out []Chapter
	var cur *Chapter
	tb := clock.Microsecond
	toUs := func(v string) (int64, error) {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
			return 0, fmt.Errorf("%w: chapter tick %q", clock.ErrInvalidTimestamp, v)
		}
		if n < 0 {
			return 0, fmt.Errorf("%w: chapter tick %d", clock.ErrInvalidTimestamp, n)
		}
		return clock.Convert(n, tb, clock.Microsecond)
	}
	flush := func() error {
		if cur == nil {
			return nil
		}
		if cur.EndUs < cur.StartUs {
			return fmt.Errorf("%w: chapter %d ends before start", clock.ErrNegativeDuration, cur.ID)
		}
		out = append(out, *cur)
		cur = nil
		return nil
	}
	for _, raw := range lines {
		line := string(bytes.TrimSuffix(raw, []byte("\r")))
		if line == ";FFMETADATA1" || line == "" {
			continue
		}
		if line == "[CHAPTER]" {
			if err := flush(); err != nil {
				return nil, err
			}
			cur = &Chapter{ID: int64(len(out) + 1), Metadata: map[string]string{}}
			tb = clock.Microsecond
			continue
		}
		k, v, ok := cutKV(line)
		if !ok {
			continue
		}
		if cur == nil {
			continue
		}
		switch k {
		case "TIMEBASE":
			var num, den int64
			if _, err := fmt.Sscanf(v, "%d/%d", &num, &den); err != nil || num <= 0 || den <= 0 {
				return nil, fmt.Errorf("%w: chapter timebase %q", clock.ErrInvalidTimeBase, v)
			}
			rat, err := clock.NewRational(num, den)
			if err != nil {
				return nil, err
			}
			tb = rat
		case "START":
			us, err := toUs(v)
			if err != nil {
				return nil, err
			}
			cur.StartUs = us
		case "END":
			us, err := toUs(v)
			if err != nil {
				return nil, err
			}
			cur.EndUs = us
		case "title":
			cur.Title = unescapeFF(v)
		default:
			cur.Metadata[k] = unescapeFF(v)
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

func cutKV(line string) (string, string, bool) {
	for i := 0; i < len(line); i++ {
		if line[i] == '=' {
			// FFmpeg escapes '=' in values; the key ends at the first '='.
			return line[:i], line[i+1:], true
		}
	}
	return "", "", false
}

func escapeFF(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '=', ';', '#', '\\', '\n':
			out = append(out, '\\', s[i])
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}

func unescapeFF(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		out = append(out, s[i])
	}
	return string(out)
}
