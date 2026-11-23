package mp4

import (
	"encoding/binary"
	"io"
	"os"

	gomp4 "github.com/abema/go-mp4"
)

// Table boxes, parsed either by go-mp4 (complete) or by a prefix decoder.
type stszData struct {
	sampleSize uint32
	count      uint32
	sizes      []uint32
	complete   bool
}

type stcoData struct {
	offsets  []uint32
	complete bool
}

type stscEntry struct {
	firstChunk      uint32
	samplesPerChunk uint32
}

type stscData struct {
	entries  []stscEntry
	complete bool
}

type sttsEntry struct {
	count uint32
	delta uint32
}

type sttsData struct {
	entries  []sttsEntry
	complete bool
}

type stssData struct {
	numbers  []uint32
	complete bool
	present  bool
}

func decodeStsc(f *os.File, b rawBox) (stscData, error) {
	if !b.truncated {
		var box gomp4.Stsc
		if _, err := gomp4.Unmarshal(io.NewSectionReader(f, b.payloadOff(), b.size-b.hdr), uint64(b.size-b.hdr), &box, gomp4.Context{}); err == nil {
			out := stscData{complete: true}
			for _, e := range box.Entries {
				out.entries = append(out.entries, stscEntry{e.FirstChunk, e.SamplesPerChunk})
			}
			return out, nil
		}
	}
	data, err := readPayload(f, b)
	if err != nil {
		return stscData{}, err
	}
	d := stscData{}
	if len(data) < 8 {
		return d, nil
	}
	count := int(binary.BigEndian.Uint32(data[4:8]))
	avail := (len(data) - 8) / 12
	n := min(count, avail)
	for i := 0; i < n; i++ {
		base := 8 + i*12
		d.entries = append(d.entries, stscEntry{
			firstChunk:      binary.BigEndian.Uint32(data[base:]),
			samplesPerChunk: binary.BigEndian.Uint32(data[base+4:]),
		})
	}
	d.complete = n == count
	return d, nil
}

func decodeStts(f *os.File, b rawBox) (sttsData, error) {
	if !b.truncated {
		var box gomp4.Stts
		if _, err := gomp4.Unmarshal(io.NewSectionReader(f, b.payloadOff(), b.size-b.hdr), uint64(b.size-b.hdr), &box, gomp4.Context{}); err == nil {
			out := sttsData{complete: true}
			for _, e := range box.Entries {
				out.entries = append(out.entries, sttsEntry{e.SampleCount, e.SampleDelta})
			}
			return out, nil
		}
	}
	data, err := readPayload(f, b)
	if err != nil {
		return sttsData{}, err
	}
	d := sttsData{}
	if len(data) < 8 {
		return d, nil
	}
	count := int(binary.BigEndian.Uint32(data[4:8]))
	avail := (len(data) - 8) / 8
	n := min(count, avail)
	for i := 0; i < n; i++ {
		base := 8 + i*8
		d.entries = append(d.entries, sttsEntry{
			count: binary.BigEndian.Uint32(data[base:]),
			delta: binary.BigEndian.Uint32(data[base+4:]),
		})
	}
	d.complete = n == count
	return d, nil
}

func decodeStss(f *os.File, b rawBox) (stssData, error) {
	if !b.truncated {
		var box gomp4.Stss
		if _, err := gomp4.Unmarshal(io.NewSectionReader(f, b.payloadOff(), b.size-b.hdr), uint64(b.size-b.hdr), &box, gomp4.Context{}); err == nil {
			return stssData{numbers: box.SampleNumber, complete: true, present: true}, nil
		}
	}
	data, err := readPayload(f, b)
	if err != nil {
		return stssData{}, err
	}
	d := stssData{present: true}
	if len(data) < 8 {
		return d, nil
	}
	count := int(binary.BigEndian.Uint32(data[4:8]))
	avail := (len(data) - 8) / 4
	n := min(count, avail)
	for i := 0; i < n; i++ {
		d.numbers = append(d.numbers, binary.BigEndian.Uint32(data[8+i*4:]))
	}
	d.complete = n == count
	return d, nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
