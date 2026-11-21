// Package manifest normalizes HLS media playlists and DASH MPD indexes onto
// the rational timeline defined by internal/model.
package manifest

import (
	"bufio"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"mediad/internal/model"
)

// ParseHLS reads an HLS media playlist from fsys and returns the confirmed
// per-track timeline. Missing fragment files or an absent ENDLIST tag produce
// a partial timeline whose Missing list names the first unresolvable entry.
func ParseHLS(fsys fs.FS, playlist string, role model.TrackRole) (*model.Timeline, error) {
	f, err := fsys.Open(playlist)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	tl := model.NewTimeline()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		header    bool
		endlist   bool
		seq       int64
		seqSet    bool
		pending   *hlsEntry
		idx       int
		cursor    = model.MustRat(0, 1)
		nextStart int64
	)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			if !header {
				return nil, fmt.Errorf("hls: missing #EXTM3U header in %s", playlist)
			}
			if pending == nil {
				return nil, fmt.Errorf("hls: URI %s without preceding EXTINF", line)
			}
			pending.uri = line
			ok, perr := commitHLSEntry(fsys, tl, role, idx, seq+int64(idx), cursor, pending)
			if perr != nil {
				return nil, perr
			}
			if !ok {
				tl.MarkPartial(model.MissingDependency{
					Track: role, Kind: model.KindHLS, Index: idx, URI: pending.uri,
					Reason: "segment file not found",
				})
				return tl, nil
			}
			next, err := cursor.Add(pending.duration)
			if err != nil {
				return nil, err
			}
			cursor = next
			nextStart = pending.byteStart + pending.byteSize
			pending = nil
			idx++
			continue
		}
		tag, params := splitTag(line)
		switch tag {
		case "EXTM3U":
			header = true
		case "EXT-X-MEDIA-SEQUENCE":
			v, err := strconv.ParseInt(params, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("hls: invalid MEDIA-SEQUENCE %q", params)
			}
			seq, seqSet = v, true
		case "EXTINF":
			d, err := parseDecimalRat(strings.TrimSuffix(strings.Split(params, ",")[0], ","))
			if err != nil {
				return nil, fmt.Errorf("hls: invalid EXTINF %q: %w", params, err)
			}
			if d.Num() <= 0 {
				return nil, fmt.Errorf("%w: EXTINF=%s", model.ErrNegativeDuration, params)
			}
			pending = &hlsEntry{duration: d}
		case "EXT-X-BYTERANGE":
			if pending == nil {
				return nil, fmt.Errorf("hls: BYTERANGE %s without EXTINF", params)
			}
			start, size, err := parseByteRange(params, nextStart)
			if err != nil {
				return nil, err
			}
			pending.byteStart, pending.byteSize = start, size
		case "EXT-X-ENDLIST":
			endlist = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !header {
		return nil, fmt.Errorf("hls: missing #EXTM3U header in %s", playlist)
	}
	if pending != nil {
		tl.MarkPartial(model.MissingDependency{
			Track: role, Kind: model.KindHLS, Index: idx, URI: pending.uri,
			Reason: "EXTINF without trailing URI",
		})
		return tl, nil
	}
	if !endlist {
		tl.MarkPartial(model.MissingDependency{
			Track: role, Kind: model.KindHLS, Index: idx, URI: "",
			Reason: "playlist missing #EXT-X-ENDLIST",
		})
	}
	_ = seqSet
	return tl, nil
}

type hlsEntry struct {
	uri       string
	duration  model.Rat
	byteStart int64
	byteSize  int64
}

func commitHLSEntry(fsys fs.FS, tl *model.Timeline, role model.TrackRole, idx int,
	mseq int64, start model.Rat, e *hlsEntry) (bool, error) {
	info, err := fs.Stat(fsys, e.uri)
	if err != nil {
		return false, nil
	}
	size := e.byteSize
	if size == 0 {
		size = info.Size()
	}
	if e.byteStart+size > info.Size() {
		return false, fmt.Errorf("hls: byte range %d+%d exceeds %s size %d",
			e.byteStart, size, e.uri, info.Size())
	}
	seg := model.SegmentRef{
		Track: role, Kind: model.KindHLS, Index: idx, URI: e.uri,
		Start: start, Duration: e.duration, ByteStart: e.byteStart,
		ByteSize: size, Sync: true, Timescale: 1,
	}
	if err := tl.Add(seg); err != nil {
		return false, err
	}
	return true, nil
}

func splitTag(line string) (tag, params string) {
	line = strings.TrimPrefix(line, "#")
	if i := strings.IndexByte(line, ':'); i >= 0 {
		return line[:i], line[i+1:]
	}
	return line, ""
}

// parseByteRange parses "n[@o]"; a missing offset means the range starts
// immediately after the previous range in the same resource.
func parseByteRange(params string, nextStart int64) (start, size int64, err error) {
	parts := strings.Split(params, "@")
	size, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil || size <= 0 {
		return 0, 0, fmt.Errorf("hls: invalid BYTERANGE %q", params)
	}
	if len(parts) == 1 {
		return nextStart, size, nil
	}
	start, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil || start < 0 {
		return 0, 0, fmt.Errorf("hls: invalid BYTERANGE offset %q", params)
	}
	return start, size, nil
}

// parseDecimalRat converts an HLS floating-point seconds tag into an exact
// rational without going through float64, so repeated runs stay bit-identical.
func parseDecimalRat(s string) (model.Rat, error) {
	s = strings.TrimSpace(s)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	parts := strings.SplitN(s, ".", 2)
	intPart, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return model.Rat{}, model.ErrOverflow
	}
	var frac, den int64 = 0, 1
	if len(parts) == 2 && parts[1] != "" {
		frac, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return model.Rat{}, model.ErrOverflow
		}
		for range parts[1] {
			den *= 10
		}
	}
	num, err := mulCheck(intPart, den)
	if err != nil {
		return model.Rat{}, err
	}
	num, err = addCheck(num, frac)
	if err != nil {
		return model.Rat{}, err
	}
	if neg {
		num = -num
	}
	return model.NewRat(num, den)
}

func mulCheck(a, b int64) (int64, error) {
	r := a * b
	if a != 0 && r/a != b {
		return 0, model.ErrOverflow
	}
	return r, nil
}

func addCheck(a, b int64) (int64, error) {
	r := a + b
	if (a^r)&(b^r) < 0 {
		return 0, model.ErrOverflow
	}
	return r, nil
}
