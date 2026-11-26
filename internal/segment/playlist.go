package segment

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/Eyevinn/hls-m3u8/m3u8"

	"avpipe/internal/media"
)

// SegmentSpec describes one timeline slot shared by every representation.
type SegmentSpec struct {
	Index    int     // segment number, starting at zero
	Duration float64 // seconds, always SegmentDurationSeconds
	Gap      bool    // true when the slot carries freeze/silence fill only
	URI      string  // file name inside the variant directory
}

// VariantPlaylist holds one rendered media playlist and its tier.
type VariantPlaylist struct {
	Tier    Tier
	Content string
}

// BuildPlaylists renders one media playlist per tier and the master
// playlist, guaranteeing identical segment counts, durations and gap flags
// across representations. Returns the master content keyed separately.
func BuildPlaylists(specs []SegmentSpec, tiers []Tier) (string, []VariantPlaylist, error) {
	hasGap := false
	for _, s := range specs {
		if s.Gap {
			hasGap = true
			break
		}
	}
	variants := make([]VariantPlaylist, 0, len(tiers))
	master := m3u8.NewMasterPlaylist()
	master.SetIndependentSegments(true)

	for _, tier := range tiers {
		pl, err := m3u8.NewMediaPlaylist(0, uint(len(specs)))
		if err != nil {
			return "", nil, media.NewError(media.ClassEncode, "segment", "playlist alloc failed: "+err.Error(), 0, false)
		}
		pl.MediaType = m3u8.VOD
		pl.TargetDuration = SegmentDurationSeconds
		dir := VariantDir(tier.Kbps)
		for _, spec := range specs {
			uri := filepath.Join(dir, spec.URI)
			if err := pl.Append(uri, spec.Duration, ""); err != nil {
				return "", nil, media.NewError(media.ClassEncode, "segment", "playlist append failed: "+err.Error(), 0, false)
			}
			if spec.Gap {
				if err := pl.SetGap(); err != nil {
					return "", nil, media.NewError(media.ClassEncode, "segment", "gap tag failed: "+err.Error(), 0, false)
				}
			}
		}
		pl.Close()
		variants = append(variants, VariantPlaylist{Tier: tier, Content: pl.Encode().String()})
		master.Append(filepath.Join(dir, "playlist.m3u8"), pl, m3u8.VariantParams{
			Bandwidth:  uint32(tier.Kbps * 1000),
			Resolution: itoa(tier.Width) + "x" + itoa(tier.Height),
			FrameRate:  float64(media.VideoFPS),
			Codecs:     "avc1.64001f,mp4a.40.2",
		})
	}
	masterContent := master.Encode().String()
	if hasGap {
		// RFC 8216 defines EXT-X-GAP at protocol version 7; the library does
		// not bump its internal version for the gap flag, so adjust both the
		// master and every variant playlist deterministically after encode.
		masterContent = bumpToVersion7(masterContent)
		for i := range variants {
			variants[i].Content = bumpToVersion7(variants[i].Content)
		}
	}
	return masterContent, variants, nil
}

// bumpToVersion7 rewrites the playlist's EXT-X-VERSION to 7.
func bumpToVersion7(content string) string {
	return strings.Replace(content, "#EXT-X-VERSION:3", "#EXT-X-VERSION:7", 1)
}

// WritePlaylists persists the master and variant playlists under outDir.
func WritePlaylists(outDir, master string, variants []VariantPlaylist) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return media.NewError(media.ClassIndex, "segment", "cannot create playlist dir: "+err.Error(), 0, false)
	}
	if err := os.WriteFile(filepath.Join(outDir, "master.m3u8"), []byte(master), 0o644); err != nil {
		return media.NewError(media.ClassIndex, "segment", "cannot write master: "+err.Error(), 0, false)
	}
	for _, v := range variants {
		dir := filepath.Join(outDir, VariantDir(v.Tier.Kbps))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return media.NewError(media.ClassIndex, "segment", "cannot create variant dir: "+err.Error(), 0, false)
		}
		if err := os.WriteFile(filepath.Join(dir, "playlist.m3u8"), []byte(v.Content), 0o644); err != nil {
			return media.NewError(media.ClassIndex, "segment", "cannot write variant playlist: "+err.Error(), 0, false)
		}
	}
	return nil
}

// VariantDir is the stable directory name for one representation.
func VariantDir(kbps int) string {
	switch kbps {
	case 180:
		return "v180k"
	case 450:
		return "v450k"
	case 900:
		return "v900k"
	default:
		return "v" + itoa(kbps) + "k"
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [16]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
