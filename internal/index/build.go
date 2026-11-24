// Package index builds sidx/tfra indices, HLS manifests and verifies random
// access locations and manifest/media timing agreement.
package index

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/abema/go-mp4"

	"bcast/internal/clock"
	"bcast/internal/media"
)

// SeekEntry is one independently locatable random-access point.
type SeekEntry struct {
	Fragment       uint32 `json:"fragment"`
	Sequence       uint32 `json:"sequence"`
	TrackID        uint32 `json:"track_id"`
	Segment        string `json:"segment"`
	MoofOffset     int64  `json:"moof_offset"`
	TimeShared     int64  `json:"time_shared"`
	DurationShared int64  `json:"duration_shared"`
	SampleIndex    int    `json:"sample_index"`
}

// Artifacts contains generated index and playlist bytes.
type Artifacts struct {
	PrimaryTrackID uint32      `json:"primary_track_id"`
	Entries        []SeekEntry `json:"entries"`
	SIDX           []byte      `json:"-"`
	TFRA           []byte      `json:"-"`
	Manifest       []byte      `json:"-"`
	IndexJSON      []byte      `json:"-"`
	ManifestPath   string      `json:"manifest_path"`
}

// Build creates sidx/tfra boxes and an HLS VOD manifest for the primary track.
func Build(tl *clock.Result) (*Artifacts, error) {
	if tl == nil || len(tl.Tracks) == 0 {
		return nil, media.Corrupt("index", "empty_timeline", "", 0, -1, "no repaired timeline to index")
	}
	trackID := primaryTrack(tl)
	tt := tl.Tracks[trackID]
	art := &Artifacts{PrimaryTrackID: trackID, ManifestPath: "manifest.m3u8"}

	for _, ft := range tt.Fragments {
		seg := filepath.Base(ft.Path)
		dur := ft.EndShared - ft.StartShared
		if len(ft.SyncIndexes) == 0 {
			return nil, media.Corrupt("index", "no_sync_sample", ft.Path, ft.Sequence, ft.MoofOffset, "fragment has no sync sample")
		}
		for _, idx := range ft.SyncIndexes {
			if idx < 0 || idx >= len(ft.Samples) {
				return nil, media.Corrupt("index", "bad_sync_index", ft.Path, ft.Sequence, ft.MoofOffset, "sync index %d out of range", idx)
			}
			art.Entries = append(art.Entries, SeekEntry{
				Fragment:       ft.OriginalFragment,
				Sequence:       ft.Sequence,
				TrackID:        trackID,
				Segment:        seg,
				MoofOffset:     ft.MoofOffset,
				TimeShared:     ft.Samples[idx].SharedDTS,
				DurationShared: dur,
				SampleIndex:    idx,
			})
		}
	}
	sort.SliceStable(art.Entries, func(i, j int) bool { return art.Entries[i].TimeShared < art.Entries[j].TimeShared })

	var err error
	if art.SIDX, err = marshalSIDX(tl, tt, art.Entries); err != nil {
		return nil, err
	}
	if art.TFRA, err = marshalTFRA(trackID, art.Entries); err != nil {
		return nil, err
	}
	if art.Manifest, err = buildManifest(tt, art.Entries); err != nil {
		return nil, err
	}
	if art.IndexJSON, err = buildIndexJSON(art); err != nil {
		return nil, err
	}
	return art, nil
}

func primaryTrack(tl *clock.Result) uint32 {
	var best uint32
	bestN := -1
	for id, tt := range tl.Tracks {
		n := 0
		for _, f := range tt.Fragments {
			n += len(f.SyncIndexes)
		}
		if n > bestN || (n == bestN && (best == 0 || id < best)) {
			best, bestN = id, n
		}
	}
	return best
}

func marshalSIDX(tl *clock.Result, tt *clock.TrackTimeline, entries []SeekEntry) ([]byte, error) {
	refs := make([]mp4.SidxReference, 0, len(tt.Fragments))
	var earliest uint64
	for i, ft := range tt.Fragments {
		if i == 0 {
			if ft.StartShared < 0 {
				return nil, media.Corrupt("index", "negative_sidx_time", ft.Path, ft.Sequence, ft.MoofOffset, "sidx earliest time is negative")
			}
			earliest = uint64(ft.StartShared)
		}
		size, err := fileSize(ft.Path)
		if err != nil {
			return nil, err
		}
		if size > uint64(^uint32(0)) {
			return nil, media.Corrupt("index", "sidx_size_overflow", ft.Path, ft.Sequence, ft.MoofOffset, "segment size %d exceeds sidx reference", size)
		}
		dur := ft.EndShared - ft.StartShared
		if dur < 0 || dur > int64(^uint32(0)) {
			return nil, media.Corrupt("index", "sidx_duration_overflow", ft.Path, ft.Sequence, ft.MoofOffset, "duration %d cannot fit sidx", dur)
		}
		refs = append(refs, mp4.SidxReference{
			ReferenceType:      false,
			ReferencedSize:     uint32(size),
			SubsegmentDuration: uint32(dur),
			StartsWithSAP:      true,
			SAPType:            1,
			SAPDeltaTime:       0,
		})
	}
	box := &mp4.Sidx{
		FullBox:                    mp4.FullBox{Version: 1},
		ReferenceID:                tt.TrackID,
		Timescale:                  uint32(media.SharedTimescale),
		EarliestPresentationTimeV1: earliest,
		FirstOffsetV1:              0,
		ReferenceCount:             uint16(len(refs)),
		References:                 refs,
	}
	out, err := marshalBox("sidx", box)
	if err != nil {
		return nil, media.Corrupt("index", "sidx_marshal", "", 0, -1, "%v", err)
	}
	return out, nil
}

func marshalTFRA(trackID uint32, entries []SeekEntry) ([]byte, error) {
	box := &mp4.Tfra{
		FullBox:               mp4.FullBox{Version: 1},
		TrackID:               trackID,
		LengthSizeOfTrafNum:   0,
		LengthSizeOfTrunNum:   0,
		LengthSizeOfSampleNum: 0,
		NumberOfEntry:         uint32(len(entries)),
	}
	for _, e := range entries {
		if e.TimeShared < 0 {
			return nil, media.Corrupt("index", "negative_tfra_time", "", e.Sequence, e.MoofOffset, "tfra time is negative")
		}
		box.Entries = append(box.Entries, mp4.TfraEntry{
			TimeV1:       uint64(e.TimeShared),
			MoofOffsetV1: uint64(e.MoofOffset),
			TrafNumber:   1,
			TrunNumber:   1,
			SampleNumber: uint32(e.SampleIndex + 1),
		})
	}
	out, err := marshalBox("tfra", box)
	if err != nil {
		return nil, media.Corrupt("index", "tfra_marshal", "", 0, -1, "%v", err)
	}
	return out, nil
}

// marshalBox adds the ISO-BMFF 8-byte box header around go-mp4's payload
// marshaller output.
func marshalBox(typ string, box mp4.IImmutableBox) ([]byte, error) {
	var payload bytes.Buffer
	if _, err := mp4.Marshal(&payload, box, mp4.Context{}); err != nil {
		return nil, err
	}
	out := make([]byte, 8+payload.Len())
	binary.BigEndian.PutUint32(out[0:4], uint32(len(out)))
	copy(out[4:8], typ)
	copy(out[8:], payload.Bytes())
	return out, nil
}

func buildManifest(tt *clock.TrackTimeline, entries []SeekEntry) ([]byte, error) {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	maxDur := 0.0
	type seg struct {
		name string
		dur  float64
	}
	seen := map[string]bool{}
	var segs []seg
	for _, e := range entries {
		if seen[e.Segment] {
			continue
		}
		seen[e.Segment] = true
		dur := media.Timebase().Seconds(e.DurationShared)
		if dur > maxDur {
			maxDur = dur
		}
		segs = append(segs, seg{e.Segment, dur})
	}
	target := int(maxDur + 0.999)
	if target < 1 {
		target = 1
	}
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", target)
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:1\n")
	for _, s := range segs {
		fmt.Fprintf(&b, "#EXTINF:%.6f,\n%s\n", s.dur, s.name)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return []byte(b.String()), nil
}

func fileSize(path string) (uint64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, media.IO("index", path, 0, 0, err)
	}
	if st.Size() < 0 {
		return 0, media.Corrupt("index", "negative_file_size", path, 0, 0, "stat returned negative size")
	}
	return uint64(st.Size()), nil
}
