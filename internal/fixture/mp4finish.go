package fixture

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"

	"github.com/abema/go-mp4"
)

// mdatBox writes one mdat box containing all generated sample data and
// returns the per-sample absolute file offsets.
func (s Spec) writeMdat(mw *mp4.Writer) ([]int64, error) {
	off, err := mw.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 8)
	binary.BigEndian.PutUint32(header[0:], 0) // size patched later
	copy(header[4:], []byte{'m', 'd', 'a', 't'})
	if _, err := mw.Write(header); err != nil {
		return nil, err
	}
	offsets := make([]int64, s.SampleCount)
	buf := make([]byte, 0, 2048)
	for i := 0; i < s.SampleCount; i++ {
		size := SampleSize(s, i)
		if cap(buf) < size {
			buf = make([]byte, size)
		}
		buf = buf[:size]
		SampleData(s, i, buf)
		pos, _ := mw.Seek(0, io.SeekCurrent)
		offsets[i] = pos
		if _, err := mw.Write(buf); err != nil {
			return nil, err
		}
	}
	end, _ := mw.Seek(0, io.SeekCurrent)
	total := uint32(end - off)
	if _, err := mw.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint32(header[0:], total)
	if _, err := mw.Write(header); err != nil {
		return nil, err
	}
	if _, err := mw.Seek(end, io.SeekStart); err != nil {
		return nil, err
	}
	return offsets, nil
}

// BuildMP4 renders the full MP4 asset to a byte slice and patches the stco
// chunk offsets so the container is self-consistent.
func (s Spec) BuildMP4() ([]byte, error) {
	f, err := os.CreateTemp("", "mediaroute-mp4-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	mw := mp4.NewWriter(f)
	if err := writeFtyp(mw); err != nil {
		return nil, err
	}
	if s.ContainerColour != nil {
		if err := writeContainerColr(mw, *s.ContainerColour); err != nil {
			return nil, err
		}
	}
	if err := s.writeMoov(mw); err != nil {
		return nil, err
	}
	offsets, err := s.writeMdat(mw)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(f.Name())
	if err != nil {
		return nil, err
	}
	if err := patchSTCO(raw, offsets); err != nil {
		return nil, err
	}
	if s.TruncateMDAT {
		raw = truncateInsideMdat(raw)
	}
	return raw, nil
}

// patchSTCO locates the stco box via recursive box descent and writes one
// chunk offset per sample.
func patchSTCO(raw []byte, offsets []int64) error {
	moov, err := findTopLevel(raw, []byte("moov"), 0, len(raw))
	if err != nil {
		return err
	}
	pos, err := descend(raw, moov,
		[]byte("trak"), []byte("mdia"), []byte("minf"),
		[]byte("stbl"), []byte("stco"))
	if err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(raw[pos:])
	count := binary.BigEndian.Uint32(raw[pos+12:])
	if int(count) != len(offsets) || pos+int(size) > len(raw) {
		return errors.New("fixture: stco layout mismatch")
	}
	for i, off := range offsets {
		binary.BigEndian.PutUint32(raw[pos+16+i*4:], uint32(off))
	}
	return nil
}

// findTopLevel returns the start offset of the first box matching want in
// the half-open region [start, end).
func findTopLevel(raw []byte, want []byte, start, end int) (int, error) {
	pos := start
	for pos+8 <= end {
		size := int(binary.BigEndian.Uint32(raw[pos:]))
		if size < 8 || pos+size > end {
			return 0, errors.New("fixture: bad box while searching " + string(want))
		}
		if bytes.Equal(raw[pos+4:pos+8], want) {
			return pos, nil
		}
		pos += size
	}
	return 0, errors.New("fixture: box not found " + string(want))
}

// descend walks one matching child box at each level and returns the
// final box start offset.
func descend(raw []byte, parentStart int, path ...[]byte) (int, error) {
	pos := parentStart
	regionStart, regionEnd := pos+8, pos+int(binary.BigEndian.Uint32(raw[pos:]))
	for _, want := range path {
		next, err := findTopLevel(raw, want, regionStart, regionEnd)
		if err != nil {
			return 0, err
		}
		pos = next
		regionStart, regionEnd = pos+8, pos+int(binary.BigEndian.Uint32(raw[pos:]))
	}
	return pos, nil
}

func truncateInsideMdat(raw []byte) []byte {
	pos := 0
	for pos+8 <= len(raw) {
		size := int(binary.BigEndian.Uint32(raw[pos:]))
		if bytes.Equal(raw[pos+4:pos+8], []byte("mdat")) {
			// Drop the last quarter of the mdat payload.
			cut := pos + size - size/4
			if cut < len(raw) {
				return raw[:cut]
			}
		}
		pos += size
	}
	return raw
}

