package manifest

import (
	"fmt"
	"strings"
)

// Loader resolves a manifest-relative URI to its bytes. It exists so that
// master playlists can be expanded without the manifest package knowing about
// the filesystem. Returning an error aborts parsing.
type Loader func(uri string) ([]byte, error)

// Parse auto-detects the manifest dialect and returns the unified group.
func Parse(data []byte, load Loader, opts Options) (*Group, error) {
	opts = opts.normalize()
	if len(data) > opts.MaxBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, len(data))
	}
	text := strings.TrimSpace(string(data))
	switch {
	case strings.HasPrefix(text, "#EXTM3U"):
		return parseHLSGroup(data, load, opts)
	case strings.Contains(text, "<MPD"), strings.Contains(text, "urn:mpeg:dash"):
		return parseDASHGroup(data, opts)
	case strings.HasPrefix(text, "<"):
		return nil, fmt.Errorf("%w: unrecognised XML manifest", ErrInvalid)
	default:
		return nil, fmt.Errorf("%w: unrecognised manifest", ErrInvalid)
	}
}

func parseHLSGroup(data []byte, load Loader, opts Options) (*Group, error) {
	doc, err := parseHLS(data, opts)
	if err != nil {
		return nil, err
	}
	g := &Group{Format: FormatHLS, Warnings: doc.warnings}
	if len(doc.variants) == 0 {
		g.Plans = doc.toPlans()
		return g, finalizeGroup(g, opts)
	}
	if load == nil {
		return nil, fmt.Errorf("%w: master playlist needs a loader", ErrInvalid)
	}
	if len(doc.variants) > opts.MaxRepresentations {
		return nil, fmt.Errorf("%w: %d variants", ErrTooLarge, len(doc.variants))
	}
	for _, v := range doc.variants {
		child, err := load(v.uri)
		if err != nil {
			return nil, fmt.Errorf("variant %s: %w", v.uri, err)
		}
		sub, err := parseHLS(child, opts)
		if err != nil {
			return nil, fmt.Errorf("variant %s: %w", v.uri, err)
		}
		plans := sub.toPlans()
		if len(plans) != 1 {
			return nil, fmt.Errorf("variant %s: %w: nested master playlist", v.uri, ErrInvalid)
		}
		pl := plans[0]
		pl.Representation = v.uri
		pl.Bandwidth = v.bandwidth
		pl.Codecs = v.codecs
		pl.Warnings = append(pl.Warnings, sub.warnings...)
		if strings.Contains(strings.ToLower(v.codecs), "mp4a") {
			pl.MimeType = "audio/mp4"
		}
		g.Plans = append(g.Plans, pl)
		g.Warnings = append(g.Warnings, sub.warnings...)
	}
	return g, finalizeGroup(g, opts)
}
