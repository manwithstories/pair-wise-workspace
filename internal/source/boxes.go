package source

import (
	"encoding/binary"
	"fmt"
	"os"

	"mediapipe/internal/media"
)

type BoxHeader struct {
	Type     string `json:"type"`
	Offset   int64  `json:"offset"`
	Size     int64  `json:"size"`
	Header   int    `json:"header_bytes"`
	Complete bool   `json:"complete"`
}

type Edit struct {
	SegmentDurationSec float64 `json:"segment_duration_sec"`
	MediaTimeSec       float64 `json:"media_time_sec"`
	MediaRate          float64 `json:"media_rate"`
}

type TrackLayout struct {
	TrackID   uint32  `json:"track_id"`
	TimeScale uint32  `json:"timescale"`
	Duration  uint64  `json:"duration"`
	OffsetSec float64 `json:"offset_sec"`
	Edits     []Edit  `json:"edits,omitempty"`
}

type BoxReport struct {
	FileSize       int64         `json:"file_size"`
	MovieTimeScale uint32        `json:"movie_timescale"`
	MoovPresent    bool          `json:"moov_present"`
	MoovComplete   bool          `json:"moov_complete"`
	Truncated      bool          `json:"truncated"`
	TruncatedBox   string        `json:"truncated_box,omitempty"`
	TopLevelBoxes  []BoxHeader   `json:"top_level_boxes"`
	Tracks         []TrackLayout `json:"tracks"`
}

// InspectBoxes parses the ISO-BMFF boxes needed to detect a truncated moov and
// read edit-list/multi-track offsets. It never repairs the file.
func InspectBoxes(path string) (*BoxReport, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, media.WrapError(media.CodeIO, "stat media", err)
	}
	lim := media.DefaultLimits()
	if err := lim.CheckInputSize(st.Size()); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, media.WrapError(media.CodeIO, "read media for box inspection", err)
	}
	rep := &BoxReport{FileSize: st.Size(), MoovComplete: false}
	for off := 0; off+8 <= len(b); {
		h, next, err := readBoxHeader(b, off)
		if err != nil {
			rep.Truncated = true
			if rep.TruncatedBox == "" {
				rep.TruncatedBox = "header"
			}
			break
		}
		if h.Offset+h.Size > int64(len(b)) {
			h.Complete = false
			rep.Truncated = true
			rep.TruncatedBox = h.Type
			rep.TopLevelBoxes = append(rep.TopLevelBoxes, h)
			if h.Type == "moov" {
				rep.MoovPresent = true
				rep.MoovComplete = false
			}
			break
		}
		rep.TopLevelBoxes = append(rep.TopLevelBoxes, h)
		if h.Type == "moov" {
			moovPayload := b[h.Offset+int64(h.Header) : h.Offset+h.Size]
			rep.MoovPresent = true
			rep.MoovComplete = true
			rep.MovieTimeScale = parseMVHDTimeScale(moovPayload)
			rep.Tracks = parseTracks(moovPayload, rep.MovieTimeScale)
		}
		off = next
	}
	if !rep.MoovPresent && len(rep.TopLevelBoxes) > 0 {
		// A real MP4/MOV cannot supply sample metadata without moov.
		rep.Truncated = true
		rep.TruncatedBox = "moov-missing"
	}
	return rep, nil
}

func readBoxHeader(b []byte, off int) (BoxHeader, int, error) {
	if off+8 > len(b) {
		return BoxHeader{}, off, fmt.Errorf("short box header at %d", off)
	}
	size32 := binary.BigEndian.Uint32(b[off : off+4])
	typ := string(b[off+4 : off+8])
	h := BoxHeader{Type: typ, Offset: int64(off), Header: 8, Size: int64(size32), Complete: true}
	next := off + int(size32)
	if size32 == 1 {
		if off+16 > len(b) {
			return h, off, fmt.Errorf("short largesize")
		}
		h.Size = int64(binary.BigEndian.Uint64(b[off+8 : off+16]))
		h.Header = 16
		next = off + int(h.Size)
	} else if size32 == 0 {
		h.Size = int64(len(b) - off)
		next = len(b)
	}
	if h.Size < int64(h.Header) {
		return h, off, fmt.Errorf("invalid box size %d", h.Size)
	}
	return h, next, nil
}

type rawEdit struct {
	segmentDuration uint64
	mediaTime       int64
	rate            float64
}

func parseTracks(moov []byte, movieTimeScale uint32) []TrackLayout {
	var tracks []TrackLayout
	walkBoxes(moov, func(typ string, payload []byte) {
		if typ != "trak" {
			return
		}
		t := TrackLayout{}
		var rawEdits []rawEdit
		findDescendants(payload, map[string]bool{"tkhd": true, "mdhd": true, "elst": true}, func(name string, p []byte) {
			switch name {
			case "tkhd":
				t.TrackID = parseTKHD(p)
			case "mdhd":
				ts, dur := parseMDHD(p)
				t.TimeScale, t.Duration = ts, dur
			case "elst":
				rawEdits = parseELSTRaw(p)
			}
		})
		movieTS := uint64(movieTimeScale)
		if movieTS == 0 {
			movieTS = 1000
		}
		for _, re := range rawEdits {
			e := Edit{MediaRate: re.rate, SegmentDurationSec: float64(re.segmentDuration) / float64(movieTS)}
			if re.mediaTime >= 0 && t.TimeScale > 0 {
				e.MediaTimeSec = float64(re.mediaTime) / float64(t.TimeScale)
				if e.MediaTimeSec > 0 && t.OffsetSec == 0 {
					t.OffsetSec = e.MediaTimeSec
				}
			}
			t.Edits = append(t.Edits, e)
		}
		tracks = append(tracks, t)
	})
	return tracks
}

func walkBoxes(b []byte, fn func(typ string, payload []byte)) {
	for off := 0; off+8 <= len(b); {
		h, next, err := readBoxHeader(b, off)
		if err != nil || next <= off || next > len(b) {
			return
		}
		start := off + h.Header
		end := off + int(h.Size)
		if end > len(b) {
			end = len(b)
		}
		if start <= end {
			fn(h.Type, b[start:end])
			// Containers are walked by callers via findDescendants where needed.
			if isContainer(h.Type) {
				walkBoxes(b[start:end], fn)
			}
		}
		off = next
	}
}

func findDescendants(b []byte, want map[string]bool, fn func(typ string, payload []byte)) {
	walkBoxes(b, func(typ string, payload []byte) {
		if want[typ] {
			fn(typ, payload)
		}
	})
}

func isContainer(typ string) bool {
	switch typ {
	case "moov", "trak", "mdia", "minf", "stbl", "edts", "udta":
		return true
	}
	return false
}

func parseTKHD(p []byte) uint32 {
	if len(p) < 16 {
		return 0
	}
	version := p[0]
	if version == 1 && len(p) >= 24 {
		return binary.BigEndian.Uint32(p[20:24])
	}
	if len(p) >= 16 {
		return binary.BigEndian.Uint32(p[12:16])
	}
	return 0
}

func parseMDHD(p []byte) (uint32, uint64) {
	if len(p) < 20 {
		return 0, 0
	}
	version := p[0]
	if version == 1 && len(p) >= 32 {
		return binary.BigEndian.Uint32(p[20:24]), binary.BigEndian.Uint64(p[24:32])
	}
	if len(p) >= 20 {
		return binary.BigEndian.Uint32(p[12:16]), uint64(binary.BigEndian.Uint32(p[16:20]))
	}
	return 0, 0
}

func parseMVHDTimeScale(moov []byte) uint32 {
	var ts uint32
	findDescendants(moov, map[string]bool{"mvhd": true}, func(_ string, p []byte) {
		if len(p) < 16 {
			return
		}
		if p[0] == 1 && len(p) >= 24 {
			ts = binary.BigEndian.Uint32(p[20:24])
		} else if len(p) >= 16 {
			ts = binary.BigEndian.Uint32(p[12:16])
		}
	})
	return ts
}

func parseELSTRaw(p []byte) []rawEdit {
	if len(p) < 8 {
		return nil
	}
	version := p[0]
	count := binary.BigEndian.Uint32(p[4:8])
	off := 8
	out := make([]rawEdit, 0, count)
	for i := uint32(0); i < count; i++ {
		var e rawEdit
		if version == 1 {
			if off+20 > len(p) {
				break
			}
			e.segmentDuration = binary.BigEndian.Uint64(p[off : off+8])
			e.mediaTime = int64(binary.BigEndian.Uint64(p[off+8 : off+16]))
			off += 20
		} else {
			if off+12 > len(p) {
				break
			}
			e.segmentDuration = uint64(binary.BigEndian.Uint32(p[off : off+4]))
			e.mediaTime = int64(int32(binary.BigEndian.Uint32(p[off+4 : off+8])))
			off += 12
		}
		e.rate = 1
		if off >= 4 {
			raw := binary.BigEndian.Uint32(p[off-4 : off])
			e.rate = float64(raw>>16) + float64(raw&0xffff)/65536
		}
		out = append(out, e)
	}
	return out
}
