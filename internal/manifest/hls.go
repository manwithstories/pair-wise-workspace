package manifest

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ParseHLS reads a master M3U8 at masterPath (an absolute local path) and the
// variant playlists it references, returning a fully expanded Manifest.
func ParseHLS(camera, masterPath string) (*Manifest, error) {
	baseDir := filepath.Dir(masterPath)
	m := &Manifest{
		Format:    FormatHLS,
		Camera:    camera,
		BaseDir:   baseDir,
		MasterURI: filepath.Base(masterPath),
	}

	tags, lines, err := readPlaylist(masterPath)
	if err != nil {
		return nil, err
	}
	if len(tags) == 0 || tags[0].name != "#EXTM3U" {
		return nil, fmt.Errorf("%w: missing #EXTM3U header", ErrInvalidManifest)
	}

	// A media playlist (single variant) vs a master playlist.
	if hasTag(tags, "#EXT-X-TARGETDURATION") {
		rep, err := parseMediaPlaylist(camera, "main", baseDir, masterPath, lines, tags)
		if err != nil {
			return nil, err
		}
		m.Representations = append(m.Representations, rep)
		return m, nil
	}

	// Master playlist: a #EXT-X-STREAM-INF tag is followed on the next line by
	// the variant playlist URI. Scan sequentially keeping the pending tag.
	var pending *plTag
	for _, raw := range lines {
		if raw == "" {
			continue
		}
		if strings.HasPrefix(raw, "#EXT") {
			name, value := splitTag(raw)
			if name == "#EXT-X-STREAM-INF" {
				t := plTag{name: name, value: value}
				pending = &t
			}
			continue
		}
		// Non-tag line: if a STREAM-INF tag is pending this is its URI.
		if pending == nil {
			continue
		}
		uri := raw
		attrs := parseAttrList(pending.value)
		bw, _ := strconv.Atoi(attrs["BANDWIDTH"])
		name := attrs["NAME"]
		if name == "" {
			name = "variant-" + strconv.Itoa(len(m.Variants))
		}
		res := attrs["RESOLUTION"]
		if _, err := safeJoin(baseDir, uri); err != nil {
			return nil, fmt.Errorf("variant %q: %w", uri, err)
		}
		m.Variants = append(m.Variants, Variant{
			URI: uri, Bandwidth: bw, Resolution: res, Name: name,
		})
		pending = nil
	}
	if len(m.Variants) == 0 {
		return nil, fmt.Errorf("%w: master playlist has no variants", ErrInvalidManifest)
	}

	// Expand each referenced variant playlist.
	for _, v := range m.Variants {
		variantPath := filepath.Join(baseDir, filepath.FromSlash(v.URI))
		vtags, vlines, err := readPlaylist(variantPath)
		if err != nil {
			return nil, err
		}
		rep, err := parseMediaPlaylist(camera, v.Name, baseDir, variantPath, vlines, vtags)
		if err != nil {
			return nil, err
		}
		rep.Bandwidth = v.Bandwidth
		if w, h, ok := parseResolution(v.Resolution); ok {
			rep.Width, rep.Height = w, h
		}
		m.Representations = append(m.Representations, rep)
	}
	return m, nil
}

// plTag is one parsed tag together with the source line index it appeared on.
type plTag struct {
	name  string
	value string
	line  int
}

func readPlaylist(p string) ([]plTag, []string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, nil, fmt.Errorf("open playlist %s: %w", p, err)
	}
	defer f.Close()

	var tags []plTag
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	idx := 0
	for sc.Scan() {
		raw := strings.TrimSpace(sc.Text())
		lines = append(lines, raw)
		if strings.HasPrefix(raw, "#EXT") {
			name, value := splitTag(raw)
			tags = append(tags, plTag{name: name, value: value, line: idx})
		}
		idx++
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("read playlist %s: %w", p, err)
	}
	return tags, lines, nil
}

func parseMediaPlaylist(camera, role, baseDir, playlistPath string, lines []string, tags []plTag) (Representation, error) {
	rep := Representation{ID: camera + "/" + role, Role: role}
	if !hasTag(tags, "#EXT-X-TARGETDURATION") {
		return rep, fmt.Errorf("%w: %s is not a media playlist", ErrInvalidManifest, playlistPath)
	}

	seqBase := 0
	if t, ok := findTag(tags, "#EXT-X-MEDIA-SEQUENCE"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(t.value)); err == nil {
			seqBase = n
		}
	}
	entry := 0
	var start90k int64
	var pendingInf *plTag
	pendingDisposition := ""
	for _, raw := range lines {
		if raw == "" {
			continue
		}
		if strings.HasPrefix(raw, "#") {
			name, value := splitTag(raw)
			switch name {
			case "#EXTINF":
				t := plTag{name: name, value: value}
				pendingInf = &t
			case "#X-DISPOSITION":
				pendingDisposition = value
			case "#X-MEDIA-SEQ":
				// Explicit per-entry sequence used by the fixture so duplicate
				// and out-of-order conditions can be expressed deterministically.
				if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
					seqBase = n
					entry = 0
				}
			}
			continue
		}
		// Segment URI line.
		uri := raw
		if _, err := safeJoin(baseDir, uri); err != nil {
			return rep, fmt.Errorf("segment %q: %w", uri, err)
		}
		dur90k := int64(0)
		if pendingInf != nil {
			d := strings.TrimSuffix(strings.TrimSpace(pendingInf.value), ",")
			if secs, err := strconv.ParseFloat(strings.TrimSpace(d), 64); err == nil {
				dur90k = int64(secs*90000 + 0.5)
			}
		}
		seq := seqBase + entry
		rep.Segments = append(rep.Segments, Segment{
			Sequence:    seq,
			URI:         filepath.ToSlash(filepath.Join(relDir(playlistPath, baseDir), uri)),
			Start90k:    start90k,
			Duration90k: dur90k,
			Disposition: pendingDisposition,
		})
		start90k += dur90k
		entry++
		pendingInf = nil
		pendingDisposition = ""
	}
	if idx, err := checkSequence(rep.Segments); err != nil {
		return rep, fmt.Errorf("%w at segment position %d in %s", err, idx, playlistPath)
	}
	return rep, nil
}

func relDir(playlistPath, baseDir string) string {
	rel, err := filepath.Rel(baseDir, filepath.Dir(playlistPath))
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

func hasTag(tags []plTag, name string) bool {
	_, ok := findTag(tags, name)
	return ok
}

func findTag(tags []plTag, name string) (plTag, bool) {
	for _, t := range tags {
		if t.name == name {
			return t, true
		}
	}
	return plTag{}, false
}

func splitTag(raw string) (string, string) {
	if i := strings.IndexByte(raw, ':'); i >= 0 {
		return raw[:i], raw[i+1:]
	}
	return raw, ""
}

func parseAttrList(v string) map[string]string {
	out := map[string]string{}
	for _, part := range splitAttrs(v) {
		if eq := strings.IndexByte(part, '='); eq >= 0 {
			key := strings.TrimSpace(part[:eq])
			val := strings.Trim(strings.TrimSpace(part[eq+1:]), `"`)
			out[key] = val
		}
	}
	return out
}

// splitAttrs splits a comma separated attribute list while keeping quoted
// values (which may themselves contain commas) intact.
func splitAttrs(v string) []string {
	var out []string
	var sb strings.Builder
	inQuote := false
	for _, r := range v {
		switch {
		case r == '"':
			inQuote = !inQuote
			sb.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, sb.String())
			sb.Reset()
		default:
			sb.WriteRune(r)
		}
	}
	if sb.Len() > 0 {
		out = append(out, sb.String())
	}
	return out
}

func parseResolution(r string) (int, int, bool) {
	parts := strings.Split(r, "x")
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(parts[0])
	h, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return w, h, true
}
