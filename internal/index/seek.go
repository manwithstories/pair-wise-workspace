package index

import (
	"bufio"
	"encoding/binary"
	"io"
	"os"
	"sort"

	"avrepair/internal/clock"
)

// frameLayout is the fixed on-disk byte size of one entry frame (excluding the
// trailing 4-byte checksum): track(2+32) + seq(4) + ts(8) + offset(8) + len(4).
const trackField = 32

func encodeEntry(e Entry) []byte {
	buf := make([]byte, 2+trackField+4+8+8+4)
	track := []byte(e.Track)
	if len(track) > trackField {
		track = track[:trackField]
	}
	binary.LittleEndian.PutUint16(buf[0:2], uint16(len(track)))
	copy(buf[2:2+trackField], track)
	off := 2 + trackField
	binary.LittleEndian.PutUint32(buf[off:off+4], uint32(e.Seq))
	binary.LittleEndian.PutUint64(buf[off+4:off+12], uint64(e.TS))
	binary.LittleEndian.PutUint64(buf[off+12:off+20], uint64(e.Offset))
	binary.LittleEndian.PutUint32(buf[off+20:off+24], uint32(e.Length))
	return buf
}

func decodeEntry(buf []byte) Entry {
	trackLen := int(binary.LittleEndian.Uint16(buf[0:2]))
	track := string(buf[2 : 2+trackLen])
	off := 2 + trackField
	return Entry{
		Track:  track,
		Seq:    int(binary.LittleEndian.Uint32(buf[off : off+4])),
		TS:     int64(binary.LittleEndian.Uint64(buf[off+4 : off+12])),
		Offset: int64(binary.LittleEndian.Uint64(buf[off+12 : off+20])),
		Length: int(binary.LittleEndian.Uint32(buf[off+20 : off+24])),
	}
}

func frameSize() int { return 2 + trackField + 4 + 8 + 8 + 4 }

// readFramed decodes one entry plus its trailing checksum from r.
func readFramed(r io.Reader) (Entry, uint32, error) {
	raw := make([]byte, frameSize())
	if _, err := io.ReadFull(r, raw); err != nil {
		return Entry{}, 0, err
	}
	var checksum uint32
	if err := binary.Read(r, binary.LittleEndian, &checksum); err != nil {
		return decodeEntry(raw), 0, err
	}
	return decodeEntry(raw), checksum, nil
}

// Count returns the number of committed entries.
func (x *Index) Count() int { return x.count }

// Path returns the committed index file location.
func (x *Index) Path() string { return x.path }

// Seek returns the entry with the greatest timestamp <= targetNS. When track
// is non-empty the search is restricted to that track. SeekMiss counts lookups
// that found no qualifying entry, which the demo asserts stays at zero for
// valid requests.
func (x *Index) Seek(track string, targetNS int64) (Entry, int, error) {
	f, err := os.Open(x.path)
	if err != nil {
		return Entry{}, 1, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<16)

	// Locate the nearest sparse anchor at or before the target to bound I/O.
	start := int64(len(Magic) + 2)
	if i := sort.Search(len(x.entries), func(i int) bool {
		return x.entries[i].TS > targetNS
	}) - 1; i >= 0 {
		start = x.entries[i].Offset
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return Entry{}, 1, err
	}
	br.Reset(f)

	var best Entry
	found := false
	miss := 0
	for {
		e, checksum, err := readFramed(br)
		if err == io.EOF {
			break
		}
		if err != nil {
			return Entry{}, 1, clock.NewError(clock.CodeTruncated, e.Track,
				clock.Interval{}, "truncated frame during seek: "+err.Error())
		}
		if crc32Check(encodeEntry(e), checksum) != nil {
			return Entry{}, 1, clock.NewError(clock.CodeChecksum, e.Track,
				clock.Interval{Start: e.TS}, "checksum failure during seek")
		}
		if track != "" && e.Track != track {
			continue
		}
		if e.TS <= targetNS {
			best, found = e, true
			continue
		}
		break // entries are globally ordered; nothing later can qualify
	}
	if !found {
		miss = 1
	}
	return best, miss, nil
}

// Entries scans the full index for reporting; intended for tests and small
// manifests rather than hot-path seeks.
func (x *Index) Entries() ([]Entry, error) {
	f, err := os.Open(x.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(int64(len(Magic)+2), io.SeekStart); err != nil {
		return nil, err
	}
	br := bufio.NewReader(f)
	var out []Entry
	for {
		e, checksum, err := readFramed(br)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if err := crc32Check(encodeEntry(e), checksum); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}
