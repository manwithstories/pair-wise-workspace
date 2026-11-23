package bitstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"

	mp4 "github.com/abema/go-mp4"

	"mediatimeline/internal/mediaerr"
)

// IndexEntry describes one sample in the keyframe/sample index.
// Timestamp is the raw (possibly wrapping) source timestamp; Digest is
// the hex SHA-256 of the exact sample bytes at [Offset, Offset+Length).
type IndexEntry struct {
	Offset    int64  `json:"offset"`
	Length    int64  `json:"length"`
	Timestamp uint64 `json:"ts"`
	Keyframe  bool   `json:"keyframe"`
	Digest    string `json:"digest"`
}

// Index is the per-track sample index carried as MP4 auxiliary
// metadata (a free box inside a minimal MP4 file).
type Index struct {
	Track      string       `json:"track"`
	Codec      string       `json:"codec"`
	TimebaseNum int64       `json:"tb_num"`
	TimebaseDen int64       `json:"tb_den"`
	Entries    []IndexEntry `json:"entries"`
}

const indexMagic = "MTIL1:"

// WriteIndexFile writes idx as an MP4 file whose free box carries the
// JSON-encoded index (auxiliary metadata container).
func WriteIndexFile(path string, idx *Index) error {
	payload, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	payload = append([]byte(indexMagic), payload...)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := mp4.NewWriter(f)
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, &mp4.Ftyp{
		MajorBrand:       [4]byte{'m', 't', 'i', 'l'},
		CompatibleBrands: []mp4.CompatibleBrandElem{{CompatibleBrand: [4]byte{'m', 't', 'i', 'l'}}},
	}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil {
		return err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFree()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, &mp4.Free{Data: payload}, mp4.Context{}); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil {
		return err
	}
	return f.Sync()
}

// ReadIndexFile reads an index MP4 file previously written by
// WriteIndexFile.
func ReadIndexFile(path string) (*Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	boxes, err := mp4.ExtractBoxWithPayload(f, nil, mp4.BoxPath{mp4.BoxTypeFree()})
	if err != nil {
		return nil, err
	}
	for _, b := range boxes {
		free, ok := b.Payload.(*mp4.Free)
		if !ok {
			continue
		}
		data := free.Data
		if len(data) < len(indexMagic) || string(data[:len(indexMagic)]) != indexMagic {
			continue
		}
		var idx Index
		if err := json.Unmarshal(data[len(indexMagic):], &idx); err != nil {
			return nil, err
		}
		return &idx, nil
	}
	return nil, mediaerr.New(mediaerr.Scan, "", 0, mediaerr.NA, true,
		"no index free box found in %s", path)
}

// DigestOf returns the hex SHA-256 of b.
func DigestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ValidateIndex cross-checks idx against the scanned samples and the
// stream bytes. Out-of-range offsets, non-monotonic offsets and digest
// mismatches are reported with the exact byte position of the offending
// entry; nothing is silently corrected.
func ValidateIndex(data []byte, samples []Sample, idx *Index, track string) error {
	if len(idx.Entries) != len(samples) {
		var off int64
		if len(idx.Entries) > 0 {
			off = idx.Entries[len(idx.Entries)-1].Offset
		}
		return mediaerr.New(mediaerr.Scan, track, off, mediaerr.NA, true,
			"index entry count %d != scanned samples %d", len(idx.Entries), len(samples))
	}
	prev := int64(-1)
	for i, e := range idx.Entries {
		if e.Offset < 0 || e.Length < 0 || e.Offset+e.Length > int64(len(data)) {
			return mediaerr.New(mediaerr.Scan, track, e.Offset, mediaerr.NA, true,
				"index entry %d out of range: [%d,%d) beyond stream size %d",
				i, e.Offset, e.Offset+e.Length, len(data))
		}
		if e.Offset <= prev {
			return mediaerr.New(mediaerr.Scan, track, e.Offset, mediaerr.NA, true,
				"index entry %d offset %d not after previous %d", i, e.Offset, prev)
		}
		prev = e.Offset
		s := samples[i]
		if e.Offset != s.Offset || e.Length != s.Length {
			return mediaerr.New(mediaerr.Scan, track, e.Offset, mediaerr.NA, true,
				"index entry %d [%d,%d) != scanned sample [%d,%d)",
				i, e.Offset, e.Offset+e.Length, s.Offset, s.Offset+s.Length)
		}
		if e.Keyframe != s.Keyframe {
			return mediaerr.New(mediaerr.Scan, track, e.Offset, mediaerr.NA, true,
				"index entry %d keyframe flag disagrees with stream", i)
		}
		if got := DigestOf(data[e.Offset : e.Offset+e.Length]); got != e.Digest {
			return mediaerr.New(mediaerr.Scan, track, e.Offset, mediaerr.NA, true,
				"index entry %d digest mismatch: index=%s stream=%s", i, e.Digest, got)
		}
	}
	return nil
}

// BuildIndex derives an index from scanned samples and raw timestamps
// (used for RTP-ingested tracks whose index comes from packet headers).
func BuildIndex(track string, codec Codec, tbNum, tbDen int64, data []byte, samples []Sample, rawTS []uint64) *Index {
	idx := &Index{Track: track, Codec: codec.String(), TimebaseNum: tbNum, TimebaseDen: tbDen}
	for i, s := range samples {
		idx.Entries = append(idx.Entries, IndexEntry{
			Offset: s.Offset, Length: s.Length, Timestamp: rawTS[i],
			Keyframe: s.Keyframe, Digest: DigestOf(data[s.Offset:s.Offset+s.Length]),
		})
	}
	return idx
}
