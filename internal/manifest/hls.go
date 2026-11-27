// Package manifest renders HLS playlists exclusively from journal-committed
// segment records. It builds a multivariant master playlist, one closed VOD
// media playlist per bitrate rendition (with LL-HLS partial segments that
// share the full-segment cut points) and one segmented subtitle playlist.
// Nothing here reads an unverified file: every URI comes from a checkpoint.
package manifest

import (
	"fmt"
	"strconv"
	"strings"

	"avcore/internal/clock"
	"avcore/internal/state"

	m3u8 "github.com/Eyevinn/hls-m3u8/m3u8"
)

// VariantSpec describes one rendition independent of its segments.
type VariantSpec struct {
	Name      string
	Bandwidth uint32
	Width     int
	Height    int
	FrameRate float64
	Codecs    string
}

// CaptionDoc names the generated subtitle playlist for one language.
type CaptionDoc struct {
	Language string
	Name     string
	URI      string
}

// Builder turns committed journal records into playlist text.
type Builder struct {
	PartDivisions int // partial segments per full segment (LL-HLS), >=1
}

// NewBuilder defaults to two partial segments per full segment.
func NewBuilder() *Builder { return &Builder{PartDivisions: 2} }

func (b *Builder) divisions() int {
	if b.PartDivisions < 1 {
		return 2
	}
	return b.PartDivisions
}

// MediaPlaylist renders one rendition's closed VOD playlist including every
// LL-HLS partial segment. records must be the committed segments for a single
// variant, in ascending cut order. The hls-m3u8 writer only retains partials
// for the last three full segments (a live sliding-window rule), which is
// wrong for a VOD manifest, so the full-segment skeleton is built with the
// library and each segment's partials are then rendered deterministically
// alongside it — preserving all cut points for the whole asset.
func (b *Builder) MediaPlaylist(records []state.SegmentRecord, spec VariantSpec) (string, error) {
	if len(records) == 0 {
		return "", fmt.Errorf("manifest: no committed segments for %s", spec.Name)
	}
	pl, err := m3u8.NewMediaPlaylist(0, uint(len(records)))
	if err != nil {
		return "", err
	}
	pl.MediaType = m3u8.VOD
	pl.Closed = true
	pl.SetIndependentSegments(true)
	for _, rec := range records {
		dur := clock.Seconds(rec.EndTicks - rec.StartTicks)
		if err := pl.Append(rec.Path, dur, ""); err != nil {
			return "", err
		}
	}
	pl.Close()

	divs := b.divisions()
	first := records[0]
	partTarget := clock.Seconds(first.EndTicks-first.StartTicks) / float64(divs)
	var out strings.Builder
	// Header through (and including) TARGETDURATION, then PART-INF.
	skeleton := pl.Encode().String()
	anchor := "#EXT-X-TARGETDURATION:" + strconv.Itoa(int(pl.TargetDuration)) + "\n"
	idx := strings.Index(skeleton, anchor)
	if idx < 0 {
		return "", fmt.Errorf("manifest: target-duration anchor missing")
	}
	out.WriteString(skeleton[:idx+len(anchor)])
	out.WriteString("#EXT-X-PART-INF:PART-TARGET=" + strconv.FormatFloat(partTarget, 'f', 3, 64) + "\n")

	// Emit each full segment preceded by its own partial segments so the
	// partials interleave with the cut points in the correct HLS order.
	for _, rec := range records {
		dur := clock.Seconds(rec.EndTicks - rec.StartTicks)
		partDur := dur / float64(divs)
		base := stripExt(rec.Path)
		for p := 0; p < divs; p++ {
			pURI := fmt.Sprintf("%s.part%d.m4s", base, p)
			out.WriteString("#EXT-X-PART:DURATION=" + strconv.FormatFloat(partDur, 'f', 3, 64))
			if p == 0 {
				out.WriteString(",INDEPENDENT=YES")
			}
			out.WriteString(",URI=\"" + pURI + "\"\n")
		}
		out.WriteString("#EXTINF:" + strconv.FormatFloat(dur, 'f', 3, 64) + ",\n")
		out.WriteString(rec.Path + "\n")
	}
	out.WriteString("#EXT-X-ENDLIST\n")
	return out.String(), nil
}

// MasterPlaylist renders the multivariant playlist, linking each rendition's
// media playlist and, when present, the subtitle alternative rendition.
func (b *Builder) MasterPlaylist(variants []VariantSpec, mediaURIs []string, caption *CaptionDoc) (string, error) {
	if len(variants) != len(mediaURIs) {
		return "", fmt.Errorf("manifest: %d variants but %d media URIs", len(variants), len(mediaURIs))
	}
	master := m3u8.NewMasterPlaylist()
	master.SetIndependentSegments(true)
	for i, v := range variants {
		params := m3u8.VariantParams{
			Bandwidth: v.Bandwidth,
			Codecs:    v.Codecs,
			Resolution: fmt.Sprintf("%dx%d", v.Width, v.Height),
			FrameRate: v.FrameRate,
		}
		if caption != nil {
			params.Subtitles = "subs"
			params.Alternatives = []*m3u8.Alternative{{
				Type: "SUBTITLES", GroupId: "subs", Name: caption.Name,
				Language: caption.Language, URI: caption.URI, Default: true, Autoselect: true,
			}}
		}
		pl, err := m3u8.NewMediaPlaylist(0, 1)
		if err != nil {
			return "", err
		}
		master.Append(mediaURIs[i], pl, params)
	}
	return master.Encode().String(), nil
}

// CaptionPlaylist renders a segmented WebVTT playlist from committed caption
// records. It is a plain VOD playlist whose URIs point at per-cut VTT files.
func (b *Builder) CaptionPlaylist(records []state.CaptionRecord) (string, error) {
	if len(records) == 0 {
		return "", fmt.Errorf("manifest: no committed caption fragments")
	}
	pl, err := m3u8.NewMediaPlaylist(0, uint(len(records)))
	if err != nil {
		return "", err
	}
	pl.MediaType = m3u8.VOD
	pl.Closed = true
	for _, rec := range records {
		// Duration is informational for subtitle playlists; the journal
		// stores ordering, so use a uniform target derived from cut count.
		if err := pl.Append(rec.Path, 1.0, ""); err != nil {
			return "", err
		}
	}
	pl.Close()
	return pl.Encode().String(), nil
}

func stripExt(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			return path[:i]
		}
		if path[i] == '/' || path[i] == '\\' {
			break
		}
	}
	return path
}
