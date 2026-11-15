package verify

import (
	"fmt"

	"m3u8ctl/internal/playlist"
	"m3u8ctl/internal/segment"
)

// SizeMismatchTolerance is the relative size-vs-bitrate tolerance: a segment
// whose measured duration differs from its declared duration by more than this
// fraction is classified as size_mismatch (separate from drift accounting).
const SizeMismatchTolerance = 0.10

// Exit codes used by the CLI.
const (
	ExitOK              = 0
	ExitCheckFailed     = 1
	ExitUsage           = 2
	ExitRoundTripFailed = 3
)

// Checker runs the verification rule set against a re-parsed master playlist
// and the original scan-derived index.
type Checker struct {
	master *playlist.MasterPlaylist
	medias map[int]*playlist.MediaPlaylist
	index  *segment.Index

	driftLimitMS int64
	failures     []FailureDetail
	roundTripOK  bool
}

// NewChecker builds a Checker. mediaPlaylists must be keyed by rendition index.
func NewChecker(master *playlist.MasterPlaylist, medias map[int]*playlist.MediaPlaylist,
	index *segment.Index, driftLimitMS int64) *Checker {
	return &Checker{
		master:       master,
		medias:       medias,
		index:        index,
		driftLimitMS: driftLimitMS,
		roundTripOK:  true,
	}
}

func (checker *Checker) addFailure(kind, file, detail string, rendition int, seq int64) {
	checker.failures = append(checker.failures, FailureDetail{
		Kind: kind, File: file, Detail: detail, Rendition: rendition, Seq: seq,
	})
}

// Run executes every rule and assembles the stable report.
func (checker *Checker) Run(duplicates int) *Report {
	checker.checkMaster()
	for _, rIndex := range checker.index.Ordered {
		if len(rIndex.Slots) == 0 {
			continue
		}
		media, ok := checker.medias[rIndex.Rendition]
		if !ok {
			checker.addFailure("missing_media", "",
				fmt.Sprintf("no media playlist for rendition %d", rIndex.Rendition),
				rIndex.Rendition, 0)
			checker.roundTripOK = false
			continue
		}
		checker.checkRoundTrip(rIndex, media)
		checker.checkDrift(rIndex, media)
		checker.checkFileHealth(rIndex, media)
	}
	checker.checkCrossRendition()

	report := checker.buildReport(duplicates)
	return report
}

func (checker *Checker) checkMaster() {
	present := 0
	for _, rIndex := range checker.index.Ordered {
		if len(rIndex.Slots) > 0 {
			present++
		}
	}
	if len(checker.master.Media) != present {
		checker.addFailure("master_refs", "", fmt.Sprintf(
			"master references %d renditions, source has %d",
			len(checker.master.Media), present), 0, 0)
		checker.roundTripOK = false
	}
	for _, ref := range checker.master.Media {
		media, ok := checker.mediasByURI(ref.URI)
		if !ok {
			checker.addFailure("master_refs", ref.URI, "referenced playlist not found", 0, 0)
			checker.roundTripOK = false
			continue
		}
		if media.BandwidthBps != ref.BandwidthBps || media.Resolution != ref.Resolution ||
			media.Name != ref.Name {
			checker.addFailure("master_refs", ref.URI,
				"stream info does not match media playlist", media.Rendition, 0)
			checker.roundTripOK = false
		}
	}
}

func (checker *Checker) mediasByURI(uri string) (*playlist.MediaPlaylist, bool) {
	for _, media := range checker.medias {
		if media.FileName == uri {
			return media, true
		}
	}
	return nil, false
}

// checkRoundTrip compares every playlist entry against the source index slice
// that falls inside the (possibly windowed) playlist sequence range.
func (checker *Checker) checkRoundTrip(rIndex *segment.RenditionIndex, media *playlist.MediaPlaylist) {
	if media.TargetDuration != int(checker.index.SlotMS/1000) {
		checker.addFailure("round_trip", media.FileName, "target duration mismatch",
			rIndex.Rendition, 0)
		checker.roundTripOK = false
	}
	indexed := make(map[int64]segment.Segment, len(rIndex.Slots))
	for seq, seg := range rIndex.Slots {
		indexed[seq] = seg
	}
	if len(media.Entries) == 0 {
		checker.addFailure("round_trip", media.FileName, "media playlist has no entries",
			rIndex.Rendition, 0)
		checker.roundTripOK = false
		return
	}
	for position, entry := range media.Entries {
		expectedSeq := media.MediaSequence + int64(position)
		if entry.Seq != expectedSeq {
			checker.addFailure("round_trip", entry.URI,
				fmt.Sprintf("entry position %d carries seq %d, want %d",
					position, entry.Seq, expectedSeq), rIndex.Rendition, expectedSeq)
			checker.roundTripOK = false
		}
		source, ok := indexed[entry.Seq]
		if entry.IsGap {
			if ok {
				checker.addFailure("round_trip", entry.URI,
					"playlist marks GAP but source has a segment", rIndex.Rendition, entry.Seq)
				checker.roundTripOK = false
			}
			continue
		}
		if !ok {
			checker.addFailure("round_trip", entry.URI,
				"playlist segment missing from source index", rIndex.Rendition, entry.Seq)
			checker.roundTripOK = false
			continue
		}
		if entry.URI != source.FileName ||
			entry.SourceTSMS != source.SourceTS ||
			entry.MeasuredMS != source.MeasuredMS ||
			entry.DeclaredMS != source.DeclaredMS {
			checker.addFailure("round_trip", entry.URI,
				"playlist entry fields differ from source index", rIndex.Rendition, entry.Seq)
			checker.roundTripOK = false
		}
	}
}

func (checker *Checker) checkDrift(rIndex *segment.RenditionIndex, media *playlist.MediaPlaylist) {
	for _, entry := range media.Entries {
		if entry.IsGap {
			continue
		}
		if seg, ok := rIndex.Slots[entry.Seq]; ok && seg.Size == 0 {
			continue
		}
		drift := entry.MeasuredMS - entry.DeclaredMS
		if drift < 0 {
			drift = -drift
		}
		if drift > checker.driftLimitMS {
			checker.addFailure("drift", entry.URI,
				fmt.Sprintf("drift %dms exceeds limit %dms", drift, checker.driftLimitMS),
				rIndex.Rendition, entry.Seq)
		}
	}
}

func (checker *Checker) checkFileHealth(rIndex *segment.RenditionIndex, media *playlist.MediaPlaylist) {
	for _, entry := range media.Entries {
		if entry.IsGap {
			continue
		}
		seg, ok := rIndex.Slots[entry.Seq]
		if !ok {
			continue
		}
		if seg.Size == 0 {
			checker.addFailure("zero_byte", seg.FileName, "segment file is empty",
				rIndex.Rendition, seg.Seq)
			continue
		}
		expectedMS := seg.DeclaredMS
		deviation := seg.MeasuredMS - expectedMS
		if deviation < 0 {
			deviation = -deviation
		}
		if float64(deviation) > float64(expectedMS)*SizeMismatchTolerance {
			checker.addFailure("size_mismatch", seg.FileName, fmt.Sprintf(
				"measured %dms vs declared %dms differs by more than %.0f%%",
				seg.MeasuredMS, seg.DeclaredMS, SizeMismatchTolerance*100),
				rIndex.Rendition, seg.Seq)
		}
	}
}

// checkCrossRendition requires every rendition playlist to expose the exact
// same sequence grid, so segment boundaries line up at any seek position.
func (checker *Checker) checkCrossRendition() {
	var reference []int64
	referenceRendition := -1
	for _, rIndex := range checker.index.Ordered {
		media, ok := checker.medias[rIndex.Rendition]
		if !ok {
			continue
		}
		grid := make([]int64, 0, len(media.Entries))
		for _, entry := range media.Entries {
			grid = append(grid, entry.Seq)
		}
		if reference == nil {
			reference, referenceRendition = grid, rIndex.Rendition
			continue
		}
		if !sameInt64(reference, grid) {
			checker.addFailure("alignment", media.FileName, fmt.Sprintf(
				"sequence grid differs from rendition %d", referenceRendition),
				rIndex.Rendition, 0)
		}
	}
}

func sameInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
