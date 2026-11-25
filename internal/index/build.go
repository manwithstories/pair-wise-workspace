// Package index builds a random-access index over repaired samples.
// Seek falls back to the previous decodable keyframe and never lands on
// a corrupt sample. Serialization is deterministic so repeated runs of
// the same input produce identical bytes (and identical sha256).
package index

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"

	mp4scan "mediaqa/internal/mp4"
)

// Entry is one indexable sample on the presentation timeline.
type Entry struct {
	PresTicks int64 // presentation ticks (after edit mapping)
	DTS       int64
	Duration  int64
	Offset    uint64 // offset in the remuxed file
	Size      uint32
	Key       bool
}

// Index is a presentation-ordered sample index for one track.
type Index struct {
	TrackID   string
	Timescale int64
	Entries   []Entry
}

// Build indexes samples by presentation time. corrupt samples are
// excluded so a seek can never land on them.
func Build(trackID string, timescale int64, samples []mp4scan.Sample, presOf func(dts int64) (int64, bool)) *Index {
	ix := &Index{TrackID: trackID, Timescale: timescale}
	for _, s := range samples {
		if s.Corrupt {
			continue
		}
		pt, ok := presOf(s.PTS)
		if !ok {
			continue // outside every edit: not addressable
		}
		ix.Entries = append(ix.Entries, Entry{
			PresTicks: pt,
			DTS:       s.DTS,
			Duration:  s.Duration,
			Offset:    s.Offset,
			Size:      s.Size,
			Key:       s.Key,
		})
	}
	sort.Slice(ix.Entries, func(i, j int) bool {
		return ix.Entries[i].PresTicks < ix.Entries[j].PresTicks
	})
	return ix
}

// Seek returns the entry to start decoding from for presentation time
// pt: the latest keyframe at or before pt. Corrupt samples never appear
// in the index, so the result is always a decodable point.
func (ix *Index) Seek(pt int64) (Entry, bool) {
	best := -1
	for i, e := range ix.Entries {
		if e.PresTicks > pt {
			break
		}
		if e.Key {
			best = i
		}
	}
	if best < 0 {
		return Entry{}, false
	}
	return ix.Entries[best], true
}

// magic identifies the index format.
const magic = "MQAIDX01"

// Marshal serializes the index deterministically.
func (ix *Index) Marshal() []byte {
	var buf bytes.Buffer
	buf.WriteString(magic)
	binary.Write(&buf, binary.BigEndian, int64(ix.Timescale))
	binary.Write(&buf, binary.BigEndian, uint32(len(ix.TrackID)))
	buf.WriteString(ix.TrackID)
	binary.Write(&buf, binary.BigEndian, uint32(len(ix.Entries)))
	for _, e := range ix.Entries {
		binary.Write(&buf, binary.BigEndian, e.PresTicks)
		binary.Write(&buf, binary.BigEndian, e.DTS)
		binary.Write(&buf, binary.BigEndian, e.Duration)
		binary.Write(&buf, binary.BigEndian, e.Offset)
		binary.Write(&buf, binary.BigEndian, e.Size)
		var k byte
		if e.Key {
			k = 1
		}
		buf.WriteByte(k)
	}
	return buf.Bytes()
}

// Unmarshal parses a serialized index.
func Unmarshal(data []byte) (*Index, error) {
	if len(data) < len(magic)+8+4 || string(data[:len(magic)]) != magic {
		return nil, fmt.Errorf("bad index magic")
	}
	r := bytes.NewReader(data[len(magic):])
	var ts int64
	var idLen, count uint32
	if err := binary.Read(r, binary.BigEndian, &ts); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.BigEndian, &idLen); err != nil {
		return nil, err
	}
	id := make([]byte, idLen)
	if _, err := r.Read(id); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.BigEndian, &count); err != nil {
		return nil, err
	}
	ix := &Index{TrackID: string(id), Timescale: ts}
	for i := uint32(0); i < count; i++ {
		var e Entry
		var k byte
		if err := binary.Read(r, binary.BigEndian, &e.PresTicks); err != nil {
			return nil, err
		}
		binary.Read(r, binary.BigEndian, &e.DTS)
		binary.Read(r, binary.BigEndian, &e.Duration)
		binary.Read(r, binary.BigEndian, &e.Offset)
		binary.Read(r, binary.BigEndian, &e.Size)
		if k, _ = r.ReadByte(); k == 1 {
			e.Key = true
		}
		ix.Entries = append(ix.Entries, e)
	}
	return ix, nil
}
