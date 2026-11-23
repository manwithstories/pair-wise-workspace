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

// decodeStsz prefers the library decoder and falls back to a prefix decode
// when the box was cut, keeping every complete 4-byte entry.
func decodeStsz(f *os.File, b rawBox) (stszData, error) {
	if !b.truncated {
		var box gomp4.Stsz
		n, err := gomp4.Unmarshal(io.NewSectionReader(f, b.payloadOff(), b.size-b.hdr), uint64(b.size-b.hdr), &box, gomp4.Context{})
		if err == nil {
			return stszData{sampleSize: box.SampleSize, count: box.SampleCount,
				sizes: box.EntrySize, complete: true}, nil
		}
		_ = n
	}
	data, err := readPayload(f, b)
	if err != nil {
		return stszData{}, err
	}
	d := stszData{}
	if len(data) < 12 {
		return d, nil
	}
	d.sampleSize = binary.BigEndian.Uint32(data[4:8])
	d.count = binary.BigEndian.Uint32(data[8:12])
	if d.sampleSize == 0 {
		avail := (len(data) - 12) / 4
		want := int(d.count)
		n := min(avail, want)
		d.sizes = make([]uint32, n)
		for i := 0; i < n; i++ {
			d.sizes[i] = binary.BigEndian.Uint32(data[12+i*4:])
		}
		d.complete = n == want
	} else {
		d.complete = true
	}
	return d, nil
}

func decodeStco(f *os.File, b rawBox) (stcoData, error) {
	if !b.truncated {
		var box gomp4.Stco
		if _, err := gomp4.Unmarshal(io.NewSectionReader(f, b.payloadOff(), b.size-b.hdr), uint64(b.size-b.hdr), &box, gomp4.Context{}); err == nil {
			return stcoData{offsets: box.ChunkOffset, complete: true}, nil
		}
	}
	data, err := readPayload(f, b)
	if err != nil {
		return stcoData{}, err
	}
	d := stcoData{}
	if len(data) < 8 {
		return d, nil
	}
	count := int(binary.BigEndian.Uint32(data[4:8]))
	avail := (len(data) - 8) / 4
	n := min(count, avail)
	d.offsets = make([]uint32, n)
	for i := 0; i < n; i++ {
		d.offsets[i] = binary.BigEndian.Uint32(data[8+i*4:])
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
