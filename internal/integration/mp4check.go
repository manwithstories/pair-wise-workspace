// Package integration verifies the full pipeline against an independent
// reading of the generated files. Nothing here trusts the production parser:
// sample tables are re-walked with a separate minimal implementation so a
// shared bug cannot make the check pass.
package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
)

// box is a top-level or nested box parsed independently of the production code.
type box struct {
	typ     string
	payload []byte
}

// parseBoxes splits buf into sibling boxes without following known types.
func parseBoxes(buf []byte) ([]box, error) {
	var out []box
	for pos := 0; pos < len(buf); {
		if len(buf)-pos < 8 {
			return out, fmt.Errorf("short box header at %d", pos)
		}
		size := int(binary.BigEndian.Uint32(buf[pos:]))
		typ := string(buf[pos+4 : pos+8])
		hdr := 8
		if size == 1 {
			if len(buf)-pos < 16 {
				return out, fmt.Errorf("short large header at %d", pos)
			}
			size = int(binary.BigEndian.Uint64(buf[pos+8:]))
			hdr = 16
		} else if size == 0 {
			size = len(buf) - pos
		}
		if size < hdr || size > len(buf)-pos {
			return out, fmt.Errorf("bad size %d for %q at %d", size, typ, pos)
		}
		out = append(out, box{typ: typ, payload: buf[pos+hdr : pos+size]})
		pos += size
	}
	return out, nil
}

func findBox(list []box, typ string) (box, bool) {
	for _, b := range list {
		if b.typ == typ {
			return b, true
		}
	}
	return box{}, false
}

// walkTrack extracts the sample tables needed to recompute the timeline.
type trackTables struct {
	trackID   uint32
	timescale uint32
	sizes     []uint32
	stts      [][2]uint32 // count, delta
	offsets   []uint32
	keys      map[uint32]bool
}

// parseMovie returns each track's raw tables from an MP4 file.
func parseMovie(path string) ([]*trackTables, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	top, err := parseBoxes(raw)
	if err != nil {
		return nil, err
	}
	moov, ok := findBox(top, "moov")
	if !ok {
		return nil, fmt.Errorf("no moov")
	}
	kids, err := parseBoxes(moov.payload)
	if err != nil {
		return nil, err
	}
	var out []*trackTables
	for _, trak := range kids {
		if trak.typ != "trak" {
			continue
		}
		t, err := parseTrak(trak.payload)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func parseTrak(payload []byte) (*trackTables, error) {
	t := &trackTables{keys: map[uint32]bool{}}
	kids, err := parseBoxes(payload)
	if err != nil {
		return nil, err
	}
	if tkhd, ok := findBox(kids, "tkhd"); ok && len(tkhd.payload) >= 16 {
		t.trackID = binary.BigEndian.Uint32(tkhd.payload[12:])
	}
	mdia, ok := findBox(kids, "mdia")
	if !ok {
		return nil, fmt.Errorf("missing mdia")
	}
	mkids, err := parseBoxes(mdia.payload)
	if err != nil {
		return nil, err
	}
	if mdhd, ok := findBox(mkids, "mdhd"); ok && len(mdhd.payload) >= 20 {
		t.timescale = binary.BigEndian.Uint32(mdhd.payload[12:])
	}
	minf, ok := findBox(mkids, "minf")
	if !ok {
		return nil, fmt.Errorf("missing minf")
	}
	nkids, err := parseBoxes(minf.payload)
	if err != nil {
		return nil, err
	}
	stbl, ok := findBox(nkids, "stbl")
	if !ok {
		return nil, fmt.Errorf("missing stbl")
	}
	skids, err := parseBoxes(stbl.payload)
	if err != nil {
		return nil, err
	}
	if b, ok := findBox(skids, "stts"); ok {
		t.stts = parseStts(b.payload)
	}
	if b, ok := findBox(skids, "stsz"); ok {
		t.sizes = parseStsz(b.payload)
	}
	if b, ok := findBox(skids, "stco"); ok {
		t.offsets = parseStco(b.payload)
	}
	if b, ok := findBox(skids, "stss"); ok {
		for _, n := range parseStss(b.payload) {
			t.keys[n] = true
		}
	}
	return t, nil
}

func parseStts(p []byte) [][2]uint32 {
	if len(p) < 8 {
		return nil
	}
	n := int(binary.BigEndian.Uint32(p[4:]))
	out := make([][2]uint32, 0, n)
	for i := 0; i < n && 8+i*8+8 <= len(p); i++ {
		c := binary.BigEndian.Uint32(p[8+i*8:])
		d := binary.BigEndian.Uint32(p[12+i*8:])
		out = append(out, [2]uint32{c, d})
	}
	return out
}

func parseStsz(p []byte) []uint32 {
	if len(p) < 12 {
		return nil
	}
	constSize := binary.BigEndian.Uint32(p[4:])
	count := int(binary.BigEndian.Uint32(p[8:]))
	out := make([]uint32, 0, count)
	if constSize != 0 {
		for i := 0; i < count; i++ {
			out = append(out, constSize)
		}
		return out
	}
	for i := 0; i < count && 12+i*4+4 <= len(p); i++ {
		out = append(out, binary.BigEndian.Uint32(p[12+i*4:]))
	}
	return out
}

func parseStco(p []byte) []uint32 {
	if len(p) < 8 {
		return nil
	}
	n := int(binary.BigEndian.Uint32(p[4:]))
	out := make([]uint32, 0, n)
	for i := 0; i < n && 8+i*4+4 <= len(p); i++ {
		out = append(out, binary.BigEndian.Uint32(p[8+i*4:]))
	}
	return out
}

func parseStss(p []byte) []uint32 {
	if len(p) < 8 {
		return nil
	}
	n := int(binary.BigEndian.Uint32(p[4:]))
	out := make([]uint32, 0, n)
	for i := 0; i < n && 8+i*4+4 <= len(p); i++ {
		out = append(out, binary.BigEndian.Uint32(p[8+i*4:]))
	}
	return out
}

// sampleRow is one independently reconstructed sample.
type sampleRow struct {
	dts    uint64
	size   uint32
	offset uint64
	key    bool
	dtsMS  int64
	endMS  int64
}

// rows expands the tables into sample rows using the production clock rules,
// reimplemented here as plain integer math.
func (t *trackTables) rows() []sampleRow {
	var out []sampleRow
	var dts uint64
	var sample uint32
	chunk := 0
	for _, run := range t.stts {
		for i := uint32(0); i < run[0]; i++ {
			if int(sample) >= len(t.sizes) {
				return out
			}
			var off uint64
			if chunk < len(t.offsets) {
				off = uint64(t.offsets[chunk])
			}
			sz := t.sizes[sample]
			row := sampleRow{
				dts:    dts,
				size:   sz,
				offset: off,
				key:    len(t.keys) == 0 || t.keys[sample+1],
			}
			row.dtsMS = ticksToMS(dts, t.timescale)
			row.endMS = ticksToMS(dts+uint64(run[1]), t.timescale)
			if row.endMS <= row.dtsMS {
				row.endMS = row.dtsMS + 1
			}
			out = append(out, row)
			chunk++
			sample++
			dts += uint64(run[1])
		}
	}
	return out
}

// ticksToMS is the shared round-half-up rule, written independently.
func ticksToMS(ticks uint64, ts uint32) int64 {
	if ts == 0 {
		return 0
	}
	return int64((ticks*1000 + uint64(ts)/2) / uint64(ts))
}

// hashFile returns the SHA-256 of a file.
func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum), nil
}

// fileSize returns the size reported by stat.
func fileSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

var _ = bytes.Equal
