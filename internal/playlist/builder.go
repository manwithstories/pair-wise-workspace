package playlist

import (
	"fmt"

	"m3u8ctl/internal/segment"
)

func gapURI(rendition int, seq int64) string {
	return fmt.Sprintf("gap-r%d-s%06d.ts", rendition, seq)
}

// BuildFromIndex turns a reconstructed per-rendition index into the master
// playlist plus one media playlist per rendition. Missing sequence slots are
// emitted as contiguous GAP entries. window > 0 enables sliding-window mode.
func BuildFromIndex(index *segment.Index, window int) (*MasterPlaylist, []*MediaPlaylist) {
	master := &MasterPlaylist{
		Version:        3,
		TargetDuration: int(index.SlotMS / 1000),
		FileName:       "master.m3u8",
	}
	medias := make([]*MediaPlaylist, 0, len(index.Ordered))
	for _, rIndex := range index.Ordered {
		if len(rIndex.Slots) == 0 {
			continue
		}
		media := &MediaPlaylist{
			Rendition:      rIndex.Rendition,
			Name:           rIndex.Profile.Name,
			BandwidthBps:   rIndex.Profile.BitrateBps,
			Resolution:     fmt.Sprintf("%dx%d", rIndex.Profile.Width, rIndex.Profile.Height),
			TargetDuration: int(index.SlotMS / 1000),
			MediaSequence:  0,
			FileName:       fmt.Sprintf("media-r%d.m3u8", rIndex.Rendition),
		}
		for seq := rIndex.MinSeq; seq <= rIndex.MaxSeq; seq++ {
			seg, ok := rIndex.Slots[seq]
			if !ok {
				media.Entries = append(media.Entries, MediaEntry{
					URI:        gapURI(rIndex.Rendition, seq),
					DurationS:  float64(index.SlotMS) / 1000,
					IsGap:      true,
					Seq:        seq,
					SourceTSMS: index.BaseTS + seq*index.SlotMS,
					MeasuredMS: index.SlotMS,
					DeclaredMS: index.SlotMS,
				})
				continue
			}
			media.Entries = append(media.Entries, MediaEntry{
				URI:        seg.FileName,
				DurationS:  float64(seg.DeclaredMS) / 1000,
				IsGap:      false,
				Seq:        seg.Seq,
				SourceTSMS: seg.SourceTS,
				MeasuredMS: seg.MeasuredMS,
				DeclaredMS: seg.DeclaredMS,
			})
		}
		ApplyWindow(media, window)
		medias = append(medias, media)
		master.Media = append(master.Media, MediaRef{
			BandwidthBps: media.BandwidthBps,
			Resolution:   media.Resolution,
			Name:         media.Name,
			URI:          media.FileName,
		})
	}
	return master, medias
}
