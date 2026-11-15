// Package seek locates the keyframe at or before a presentation
// timestamp and hashes a run of frames in decode order, reading mdat
// strictly by offset (never whole-file loads).
package seek

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"

	"mp4probe/internal/box"
	"mp4probe/internal/index"
	"mp4probe/internal/timeline"
)

// ErrLocate marks "no locatable keyframe / out of range" failures
// (exit code 5).
var ErrLocate = errors.New("cannot locate keyframe")

func locatef(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrLocate, fmt.Sprintf(format, args...))
}

// FrameInfo describes one hashed sample.
type FrameInfo struct {
	SampleIndex int    `json:"sample_index"`
	PTS         int64  `json:"pts"` // media pts, track timescale
	DTS         int64  `json:"dts"`
	FrameType   string `json:"frame_type"` // "sync" or "non-sync"
	Offset      int64  `json:"offset"`
	Size        uint32 `json:"size"`
	SHA256      string `json:"sha256"`
}

// Result is the outcome of one seek.
type Result struct {
	TrackID              uint32      `json:"track_id"`
	RequestedPts         int64       `json:"requested_pts"` // presentation, track timescale
	LandingPts           int64       `json:"landing_pts"`   // presentation, track timescale
	LandingDts           int64       `json:"landing_dts"`
	LandingSampleIndex   int         `json:"landing_sample_index"`
	FrameType            string      `json:"frame_type"`
	SampleOffset         int64       `json:"sample_offset"`
	SampleSize           uint32      `json:"sample_size"`
	SHA256               string      `json:"sha256"`
	DecodePathLength     int         `json:"decode_path_length"`
	SnappedFromEmptyEdit bool        `json:"snapped_from_empty_edit,omitempty"`
	Truncated            bool        `json:"truncated,omitempty"`
	Frames               []FrameInfo `json:"frames"`
}

// Locator seeks within one indexed track.
type Locator struct {
	Tr     *index.TrackIndex
	Mapper *timeline.Mapper
	order  []int // sample indices sorted by pts
	r      io.ReaderAt
}

// NewLocator builds a Locator for trackID. The mapper converts between
// the presentation timeline (movie timescale) and media timestamps.
func NewLocator(idx *index.Index, trackID uint32, mapper *timeline.Mapper, r io.ReaderAt) (*Locator, error) {
	tr := idx.Track(trackID)
	if tr == nil {
		return nil, locatef("track %d not found", trackID)
	}
	return &Locator{
		Tr:     tr,
		Mapper: mapper,
		order:  timeline.PresentationOrder(tr.PTS),
		r:      r,
	}, nil
}

// IsSync reports whether sample i is a sync sample.
func (l *Locator) IsSync(i int) bool {
	if l.Tr.AllSync {
		return true
	}
	n := sort.Search(len(l.Tr.Sync), func(k int) bool { return int(l.Tr.Sync[k]) >= i })
	return n < len(l.Tr.Sync) && int(l.Tr.Sync[n]) == i
}

// frameType labels a sample.
func (l *Locator) frameType(i int) string {
	if l.IsSync(i) {
		return "sync"
	}
	return "non-sync"
}

// HashSample reads exactly size bytes at off and returns the hex
// SHA-256. A short read is a structural error, never silently accepted.
func HashSample(r io.ReaderAt, off int64, size uint32) (string, error) {
	buf := make([]byte, size)
	n, err := r.ReadAt(buf, off)
	if n != len(buf) {
		return "", box.ErrTruncated("hash sample", uint64(off), uint64(size), uint64(n))
	}
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}

// findByPts returns the sample whose pts is the greatest <= media, in
// presentation order (B-frame safe).
func (l *Locator) findByPts(media int64) (int, error) {
	pts := l.Tr.PTS
	n := sort.Search(len(l.order), func(k int) bool {
		return pts[l.order[k]] > media
	})
	if n == 0 {
		return 0, locatef("target media time %d precedes first sample pts %d", media, pts[l.order[0]])
	}
	return l.order[n-1], nil
}

// landingSync returns the greatest sync sample index <= target.
func (l *Locator) landingSync(target int) (int, error) {
	if l.Tr.AllSync {
		return target, nil
	}
	if len(l.Tr.Sync) == 0 {
		return 0, locatef("track %d: stss present but empty", l.Tr.TrackID)
	}
	n := sort.Search(len(l.Tr.Sync), func(k int) bool { return int(l.Tr.Sync[k]) > target })
	if n == 0 {
		return 0, locatef("track %d: target sample %d precedes first sync sample %d",
			l.Tr.TrackID, target, l.Tr.Sync[0])
	}
	return int(l.Tr.Sync[n-1]), nil
}

// Locate maps targetPres (movie timescale) to media time, finds the
// target sample, backs up to the landing keyframe and hashes nFrames
// frames in decode order starting at the landing frame.
func (l *Locator) Locate(targetPres int64, nFrames int) (*Result, error) {
	tr := l.Tr
	if tr.MediaDuration == 0 {
		return nil, locatef("track %d: zero-duration track", tr.TrackID)
	}
	if tr.SampleCount == 0 {
		return nil, locatef("track %d: no samples", tr.TrackID)
	}
	if tr.SampleCount == 1 {
		return nil, locatef("track %d: single-sample track", tr.TrackID)
	}
	if targetPres < 0 {
		return nil, locatef("negative target %d", targetPres)
	}
	if len(l.Mapper.Segs) > 0 && targetPres >= l.Mapper.TotalPres {
		return nil, locatef("target %d beyond presentation duration %d", targetPres, l.Mapper.TotalPres)
	}
	media, ok := l.Mapper.ToMedia(targetPres)
	res := &Result{TrackID: tr.TrackID}
	if !ok {
		media, ok = l.Mapper.SnapForward(targetPres)
		if !ok {
			return nil, locatef("target %d falls in an unmapped edit", targetPres)
		}
		res.SnappedFromEmptyEdit = true
	}
	if media >= int64(tr.MediaDuration) {
		return nil, locatef("target media %d beyond media duration %d", media, tr.MediaDuration)
	}
	target, err := l.findByPts(media)
	if err != nil {
		return nil, err
	}
	landing, err := l.landingSync(target)
	if err != nil {
		return nil, err
	}
	res.DecodePathLength = target - landing + 1
	// Presentation pts of the landing frame, reported in track
	// timescale so it is directly comparable to requested_pts.
	landingPres, ok := l.Mapper.ToPresentation(tr.PTS[landing])
	if !ok {
		landingPres = tr.PTS[landing]
	}
	res.LandingPts = landingPres * int64(l.Mapper.TrackScale) / int64(l.Mapper.MovieScale)
	res.LandingDts = tr.DTS[landing]
	res.LandingSampleIndex = landing
	res.FrameType = l.frameType(landing)
	res.SampleOffset = tr.Offsets[landing]
	res.SampleSize = tr.Sizes[landing]
	hash, err := HashSample(l.r, tr.Offsets[landing], tr.Sizes[landing])
	if err != nil {
		return nil, err
	}
	res.SHA256 = hash
	// Frames in decode order starting at the landing frame.
	count := nFrames
	if landing+count > tr.SampleCount {
		count = tr.SampleCount - landing
		res.Truncated = true
	}
	res.Frames = make([]FrameInfo, 0, count)
	for i := landing; i < landing+count; i++ {
		h, err := HashSample(l.r, tr.Offsets[i], tr.Sizes[i])
		if err != nil {
			return nil, err
		}
		res.Frames = append(res.Frames, FrameInfo{
			SampleIndex: i,
			PTS:         tr.PTS[i],
			DTS:         tr.DTS[i],
			FrameType:   l.frameType(i),
			Offset:      tr.Offsets[i],
			Size:        tr.Sizes[i],
			SHA256:      h,
		})
	}
	return res, nil
}
