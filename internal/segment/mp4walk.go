// Package segment validates fragmented MP4 inputs, locates structural damage
// at byte precision and plans keyframe-aligned cuts on the unified timeline.
package segment

import (
	"encoding/binary"
	"io"

	"github.com/abema/go-mp4"

	"mediad/internal/model"
)

// BoxRecord describes one box located by the streaming walker.
type BoxRecord struct {
	Type       string `json:"type"`
	Offset     int64  `json:"offset"`
	HeaderSize int64  `json:"header_size"`
	Size       int64  `json:"size"`
	Depth      int    `json:"depth"`
}

// WalkReport is the structural evidence collected for one fragment.
type WalkReport struct {
	Boxes     []BoxRecord `json:"boxes"`
	HasMoof   bool        `json:"has_moof"`
	TfhdCount int         `json:"tfhd_count"`
	// NonSync is set only when a tfhd explicitly flags a non-sync sample.
	NonSync bool `json:"non_sync"`
}

// Sync reports whether the fragment may serve as a keyframe cut boundary.
// Absence of negative evidence (plain audio / init-only fragments) counts
// as sync; the splitter still rejects boundaries whose evidence is missing
// when the caller demanded certainty.
func (r *WalkReport) Sync() bool { return !r.NonSync }

var containerTypes = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true,
	"edts": true, "dinf": true, "mvex": true, "udta": true, "iprp": true,
	"moof": true, "traf": true, "mfra": true, "meco": true,
}

// WalkStream walks every box in [0, limit) of r using only seeks and small
// header reads, so peak memory is independent of fragment size. The first
// structural defect is returned as a model.MediaError carrying the exact
// byte offset and recoverability.
func WalkStream(r io.ReadSeeker, limit int64, track model.TrackRole, segIdx int) (*WalkReport, error) {
	rep := &WalkReport{}
	if err := walkBoxes(r, 0, limit, 0, track, segIdx, rep); err != nil {
		return nil, err
	}
	// Box headers alone cannot prove that declared payloads are physically
	// present; locate the first unreadable byte so tail truncation inside the
	// last box is still reported at byte precision.
	if bad, ok := firstUnreadable(r, limit); !ok {
		return rep, model.NewMediaError("walk", track, int64(segIdx), bad, true, model.ErrTruncated)
	}
	return rep, nil
}

// firstUnreadable returns the smallest offset in [0,limit) whose byte cannot
// be read. The second result is false when such an offset exists.
func firstUnreadable(r io.ReadSeeker, limit int64) (int64, bool) {
	if limit <= 0 {
		return 0, true
	}
	if readableAt(r, limit-1) {
		return 0, true
	}
	good, bad := int64(-1), limit
	for bad-good > 1 {
		mid := good + (bad-good)/2
		if readableAt(r, mid) {
			good = mid
		} else {
			bad = mid
		}
	}
	return bad, false
}

func readableAt(r io.ReadSeeker, off int64) bool {
	if err := seek(r, off); err != nil {
		return false
	}
	_, err := io.ReadFull(r, make([]byte, 1))
	return err == nil
}

func walkBoxes(r io.ReadSeeker, base, limit int64, depth int,
	track model.TrackRole, segIdx int, rep *WalkReport) error {
	pos := base
	for pos < limit {
		if err := seek(r, pos); err != nil {
			return err
		}
		hdr := make([]byte, 8)
		n, err := io.ReadFull(r, hdr)
		if err == io.ErrUnexpectedEOF || err == io.EOF {
			// A fragment ending mid-header is tail truncation; the prefix
			// already walked is usable, so the error is recoverable.
			return model.NewMediaError("walk", track, int64(segIdx), pos+int64(n), true, model.ErrTruncated)
		}
		if err != nil {
			return model.NewMediaError("walk", track, int64(segIdx), pos, false, err)
		}
		size32 := int64(binary.BigEndian.Uint32(hdr[:4]))
		btype := string(hdr[4:])
		hdrSize := int64(8)
		size := size32
		switch size32 {
		case 0: // box extends to end of fragment
			size = limit - pos
		case 1: // 64-bit largesize
			ext := make([]byte, 8)
			en, eerr := io.ReadFull(r, ext)
			if eerr != nil {
				return model.NewMediaError("walk", track, int64(segIdx), pos+8+int64(en), true, model.ErrTruncated)
			}
			size = int64(binary.BigEndian.Uint64(ext))
			hdrSize = 16
		}
		if size < hdrSize {
			return model.NewMediaError("walk", track, int64(segIdx), pos, false, model.ErrBadSegment)
		}
		end := pos + size
		if end > limit || end < pos {
			return model.NewMediaError("walk", track, int64(segIdx), pos, true, model.ErrBoxOutOfRange)
		}
		rep.Boxes = append(rep.Boxes, BoxRecord{
			Type: btype, Offset: pos, HeaderSize: hdrSize, Size: size, Depth: depth,
		})
		if btype == "moof" {
			rep.HasMoof = true
		}
		if btype == "tfhd" {
			rep.TfhdCount++
			if err := inspectTfhd(r, pos+hdrSize, size-hdrSize, rep); err != nil {
				return model.NewMediaError("walk", track, int64(segIdx), pos, false, err)
			}
		}
		if containerTypes[btype] {
			if err := walkBoxes(r, pos+hdrSize, end, depth+1, track, segIdx, rep); err != nil {
				return err
			}
		}
		pos = end
	}
	return nil
}

// nonSyncSampleFlag is sample_is_non_sync_sample (ISO/IEC 14496-12, 8.6.3).
const nonSyncSampleFlag uint32 = 0x00010000

// defaultSampleFlagsPresent is tfhd flag 0x000020.
const defaultSampleFlagsPresent uint32 = 0x000020

func inspectTfhd(r io.ReadSeeker, payload, payloadSize int64, rep *WalkReport) error {
	if err := seek(r, payload); err != nil {
		return err
	}
	var tfhd mp4.Tfhd
	if _, err := mp4.Unmarshal(r, uint64(payloadSize), &tfhd, mp4.Context{}); err != nil {
		return err
	}
	flags := uint32(tfhd.Flags[0])<<16 | uint32(tfhd.Flags[1])<<8 | uint32(tfhd.Flags[2])
	if flags&defaultSampleFlagsPresent != 0 && tfhd.DefaultSampleFlags&nonSyncSampleFlag != 0 {
		rep.NonSync = true
	}
	return nil
}

func seek(r io.Seeker, off int64) error {
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return err
	}
	return nil
}
