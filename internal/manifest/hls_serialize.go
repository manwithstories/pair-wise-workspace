package manifest

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"mediad/internal/clock"
)

// SerializeHLS writes a media playlist that re-parses to the same model.
func SerializeHLS(m *Manifest) ([]byte, error) {
	if m == nil || m.Kind != KindHLS {
		return nil, errors.New("manifest: not an HLS manifest")
	}
	base := clock.Default()
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	if m.Version != 0 {
		fmt.Fprintf(&b, "#EXT-X-VERSION:%d\n", m.Version)
	}
	if m.TargetDuration != 0 {
		fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n",
			(m.TargetDuration+int64(base.Timescale)/2)/int64(base.Timescale))
	}
	if m.MediaSequence != 0 {
		fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", m.MediaSequence)
	}
	if m.PlaylistType != "" {
		fmt.Fprintf(&b, "#EXT-X-PLAYLIST-TYPE:%s\n", m.PlaylistType)
	}
	for _, rep := range m.Representations {
		var lastMap, lastKey string
		for _, seg := range rep.Segments {
			if seg.Discontinuity {
				b.WriteString("#EXT-X-DISCONTINUITY\n")
			}
			if seg.MapURI != "" {
				sig := seg.MapURI + "|" + rangeString(seg.MapByteRange)
				if sig != lastMap {
					fmt.Fprintf(&b, "#EXT-X-MAP:URI=%s", hlsQuote(seg.MapURI))
					if seg.MapByteRange != nil {
						fmt.Fprintf(&b, ",BYTERANGE=%s", hlsQuote(rangeString(seg.MapByteRange)))
					}
					b.WriteString("\n")
					lastMap = sig
				}
			}
			if seg.Key != nil {
				sig := keySignature(seg.Key)
				if sig != lastKey {
					fmt.Fprintf(&b, "#EXT-X-KEY:METHOD=%s", hlsQuote(seg.Key.Method))
					if seg.Key.URI != "" {
						fmt.Fprintf(&b, ",URI=%s", hlsQuote(seg.Key.URI))
					}
					if seg.Key.IV != "" {
						fmt.Fprintf(&b, ",IV=%s", hlsQuote(seg.Key.IV))
					}
					if seg.Key.KeyFormat != "" {
						fmt.Fprintf(&b, ",KEYFORMAT=%s", hlsQuote(seg.Key.KeyFormat))
					}
					if seg.Key.KeyFormatVersions != "" {
						fmt.Fprintf(&b, ",KEYFORMATVERSIONS=%s", hlsQuote(seg.Key.KeyFormatVersions))
					}
					b.WriteString("\n")
					lastKey = sig
				}
			}
			if seg.DurationKnown {
				fmt.Fprintf(&b, "#EXTINF:%s,\n", clock.FormatDecimalTicks(seg.Duration, base.Timescale, 3))
			}
			if seg.ByteRange != nil {
				rs := strconv.FormatInt(seg.ByteRange.Length, 10)
				if seg.ByteRange.HasOffset {
					rs += "@" + strconv.FormatInt(seg.ByteRange.Offset, 10)
				}
				fmt.Fprintf(&b, "#EXT-X-BYTERANGE:%s\n", rs)
			}
			b.WriteString(seg.URI)
			b.WriteString("\n")
		}
	}
	if m.EndList {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return []byte(b.String()), nil
}

func rangeString(r *Range) string {
	if r == nil {
		return ""
	}
	s := strconv.FormatInt(r.Length, 10)
	if r.HasOffset {
		s += "@" + strconv.FormatInt(r.Offset, 10)
	}
	return s
}

func keySignature(k *Key) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s", k.Method, k.URI, k.IV, k.KeyFormat, k.KeyFormatVersions)
}

func hlsQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
