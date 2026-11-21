package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"

	mp4 "github.com/abema/go-mp4"

	"mediacore/internal/media"
)

// MP4Adapter parses standard ISO base media files. It uses go-mp4 for box
// definitions but walks the tree itself so that a truncated or oversized box
// is reported with the exact byte offset instead of a generic parse error.
type MP4Adapter struct{}

func (*MP4Adapter) Format() string { return "mp4" }

// unknown counts a skipped box on the walker so the container reports how
// much structure it did not interpret.
func (a *MP4Adapter) unknown(w *mp4Walker) { w.unknown++ }
func (*MP4Adapter) Version() string        { return "1.0.0" }

func (*MP4Adapter) Detect(head []byte, size int64) bool {
	return len(head) >= 12 && string(head[4:8]) == "ftyp"
}

// Parse walks the top-level boxes, then movie and track boxes.
func (a *MP4Adapter) Parse(ctx context.Context, src media.Source) (*media.Container, error) {
	if err := ctx.Err(); err != nil {
		return nil, media.NewError(media.ClassCancelled, "cancelled before parse").WithCause(err)
	}
	w := &mp4Walker{src: src, size: src.Size(), format: "mp4"}
	root, err := w.walk(0, w.size, 0)
	if err != nil {
		return nil, err
	}

	moov := firstBox(root, "moov")
	if moov == nil {
		return nil, media.NewError(media.ClassMalformed, "no moov box found").WithContainer("mp4")
	}
	c := &media.Container{Format: "mp4", Path: src.Name(), Size: w.size}
	if err := a.parseMovie(w, moov, c); err != nil {
		return nil, err
	}
	c.UnknownBoxes = w.unknown
	c.Fingerprint, err = fingerprint(src, c)
	if err != nil {
		return nil, err
	}
	media.SortTracks(c.Tracks)
	return c, nil
}

func (a *MP4Adapter) parseMovie(w *mp4Walker, moov *boxNode, c *media.Container) error {
	kids, err := w.children(moov)
	if err != nil {
		return err
	}
	mvhd := firstBox(kids, "mvhd")
	if mvhd == nil {
		return media.NewError(media.ClassMalformed, "moov has no mvhd").WithContainer("mp4").WithOffset(moov.off)
	}
	// Unknown boxes inside moov are counted, not interpreted. The set of
	// types this adapter understands is explicit so an unexpected box is
	// never silently treated as known.
	known := map[string]bool{
		"mvhd": true, "trak": true, "udta": true, "meta": true,
		"mvex": true, "iods": true, "free": true, "skip": true, "wide": true,
	}
	for _, k := range kids {
		if !known[k.typ] {
			a.unknown(w)
		}
	}
	traks := allBoxes(kids, "trak")
	if len(traks) == 0 {
		return media.NewError(media.ClassMalformed, "moov has no tracks").WithContainer("mp4").WithOffset(moov.off)
	}
	for _, trak := range traks {
		t, err := a.parseTrack(w, trak)
		if err != nil {
			return err
		}
		c.Tracks = append(c.Tracks, t)
	}

	// Container-level geometry: a movie matrix rotation applies when no
	// track-level matrix is present.
	if m, ok, err := readMatrix(w, mvhd); err != nil {
		return err
	} else if ok && m != 0 {
		c.Rotation, c.HasRotate = m, true
	}
	return nil
}

func (a *MP4Adapter) parseTrack(w *mp4Walker, trak *boxNode) (*media.Track, error) {
	trakKids, err := w.children(trak)
	if err != nil {
		return nil, err
	}
	tkhd := firstBox(trakKids, "tkhd")
	if tkhd == nil {
		return nil, media.NewError(media.ClassMalformed, "trak has no tkhd").WithContainer("mp4").WithOffset(trak.off)
	}
	id, err := readTrackID(w, tkhd)
	if err != nil {
		return nil, err
	}
	if id == 0 {
		return nil, media.NewError(media.ClassMissingTrackID, "tkhd track_ID is zero").
			WithContainer("mp4").WithOffset(tkhd.off)
	}
	t := &media.Track{ID: media.TrackID{Container: "mp4", Value: id}, Type: media.MediaOther}
	t.Width, t.Height, err = readTkhdSize(w, tkhd)
	if err != nil {
		return nil, err
	}
	if rot, ok, err := readMatrix(w, tkhd); err != nil {
		return nil, err
	} else if ok {
		t.Rotation, t.HasRotate = rot, true
	}

	mdia := firstBox(trakKids, "mdia")
	if mdia == nil {
		return nil, media.NewError(media.ClassMalformed, "trak has no mdia").
			WithContainer("mp4").WithTrack(id).WithOffset(trak.off)
	}
	mdiaKids, err := w.children(mdia)
	if err != nil {
		return nil, err
	}
	mdhd := firstBox(mdiaKids, "mdhd")
	hdlr := firstBox(mdiaKids, "hdlr")
	minf := firstBox(mdiaKids, "minf")
	if mdhd == nil || hdlr == nil || minf == nil {
		return nil, media.NewError(media.ClassMalformed, "mdia is missing mdhd, hdlr or minf").
			WithContainer("mp4").WithTrack(id).WithOffset(mdia.off)
	}
	minfKids, err := w.children(minf)
	if err != nil {
		return nil, err
	}
	stbl := firstBox(minfKids, "stbl")
	if stbl == nil {
		return nil, media.NewError(media.ClassMalformed, "minf has no stbl").
			WithContainer("mp4").WithTrack(id).WithOffset(minf.off)
	}
	if _, err := w.children(stbl); err != nil {
		return nil, err
	}

	handler, err := readHandler(w, hdlr)
	if err != nil {
		return nil, err
	}
	t.Type = mediaTypeForHandler(handler)

	timescale, duration, err := readMdhd(w, mdhd)
	if err != nil {
		return nil, err
	}
	t.Timescale = timescale
	if timescale == 0 {
		return nil, media.NewError(media.ClassMalformed, "mdhd timescale is zero").
			WithContainer("mp4").WithTrack(id).WithOffset(mdhd.off)
	}
	t.Duration = media.RatFromTicks(duration, timescale)

	codec, err := a.parseSampleDescription(w, stbl, t)
	if err != nil {
		return nil, err
	}
	t.Codec = codec

	index, err := parseSampleTable(w, stbl, timescale, id)
	if err != nil {
		return nil, err
	}
	if err := index.Validate(); err != nil {
		return nil, media.WithTrackContext(err, "mp4", id)
	}
	t.Index = index
	return t, nil
}

// parseSampleDescription reads the first stsd entry and reports unsupported
// codecs explicitly.
func (a *MP4Adapter) parseSampleDescription(w *mp4Walker, stbl *boxNode, t *media.Track) (string, error) {
	stsd := stbl.first("stsd")
	if stsd == nil {
		return "", media.NewError(media.ClassMalformed, "stbl has no stsd").
			WithContainer("mp4").WithTrack(t.ID.Value).WithOffset(stbl.off)
	}
	// stsd is a full box: version/flags then a 32-bit entry count.
	entries, err := w.childrenFrom(stsd, stsd.payload+8)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", media.NewError(media.ClassMalformed, "stsd has no sample entries").
			WithContainer("mp4").WithTrack(t.ID.Value).WithOffset(stsd.off)
	}
	entry := entries[0]
	codec := entry.typ
	if !supportedCodec(codec) {
		return "", media.NewError(media.ClassUnsupportedCodec, "codec %q is outside the supported set", codec).
			WithContainer("mp4").WithTrack(t.ID.Value).WithOffset(entry.off).WithTolerance(0)
	}
	// Sample entries carry a fixed prefix before their child boxes.
	start := entry.payload + int64(entryPrefixLen(codec))
	if start > entry.end() {
		return "", media.NewError(media.ClassInvalidLength,
			"sample entry %q is shorter than its fixed header", codec).
			WithContainer("mp4").WithTrack(t.ID.Value).WithOffset(entry.off).WithTolerance(0)
	}
	if _, err := w.childrenFrom(entry, start); err != nil {
		return "", err
	}
	t.SampleEntry = w.rawBox(entry)
	t.Color = readEntryColor(w, entry)
	crop, err := readEntryCrop(w, entry, t.Width, t.Height, t.ID.Value)
	if err != nil {
		return "", err
	}
	t.Crop = crop
	return codec, nil
}

func supportedCodec(c string) bool {
	switch c {
	case "avc1", "avc3", "hvc1", "hev1", "mp4v", "av01", "vp09", "mp4a", "opus", "ac-3", "lpcm":
		return true
	default:
		return false
	}
}

// entryPrefixLen is the size of the type-specific sample entry header that
// precedes child boxes.
func entryPrefixLen(codec string) int {
	switch codec {
	case "mp4a", "opus", "ac-3", "lpcm":
		return 28
	default:
		return 78
	}
}

func mediaTypeForHandler(h string) media.MediaType {
	switch h {
	case "vide":
		return media.MediaVideo
	case "soun":
		return media.MediaAudio
	default:
		return media.MediaOther
	}
}

func readHandler(w *mp4Walker, n *boxNode) (string, error) {
	var h mp4.Hdlr
	if err := w.unmarshal(n, &h); err != nil {
		return "", err
	}
	return string(h.HandlerType[:]), nil
}

func readTrackID(w *mp4Walker, n *boxNode) (uint32, error) {
	var b mp4.Tkhd
	if err := w.unmarshal(n, &b); err != nil {
		return 0, err
	}
	return b.TrackID, nil
}

func readTkhdSize(w *mp4Walker, n *boxNode) (int, int, error) {
	var b mp4.Tkhd
	if err := w.unmarshal(n, &b); err != nil {
		return 0, 0, err
	}
	return int(b.Width >> 16), int(b.Height >> 16), nil
}

func readMdhd(w *mp4Walker, n *boxNode) (uint32, int64, error) {
	var b mp4.Mdhd
	if err := w.unmarshal(n, &b); err != nil {
		return 0, 0, err
	}
	var dur uint64
	if b.GetVersion() == 1 {
		dur = b.DurationV1
	} else {
		dur = uint64(b.DurationV0)
	}
	if dur > 1<<62 {
		return 0, 0, media.NewError(media.ClassInvalidLength, "mdhd duration is out of range").
			WithOffset(n.off)
	}
	return b.Timescale, int64(dur), nil
}

// rotationFromMatrix converts a display matrix to counter-clockwise quarter
// turns. Anything that is not a clean quarter turn is reported rather than
// rounded.
func rotationFromMatrix(m [9]int32) (int, bool, error) {
	one := int32(0x00010000)
	switch {
	case m[0] == one && m[1] == 0 && m[3] == 0 && m[4] == one:
		return 0, true, nil
	case m[0] == 0 && m[1] == one && m[3] == -one && m[4] == 0:
		return 1, true, nil
	case m[0] == -one && m[1] == 0 && m[3] == 0 && m[4] == -one:
		return 2, true, nil
	case m[0] == 0 && m[1] == -one && m[3] == one && m[4] == 0:
		return 3, true, nil
	default:
		return 0, false, media.NewError(media.ClassMetadataConflict,
			"display matrix is not a quarter-turn rotation: [%d %d %d %d]", m[0], m[1], m[3], m[4])
	}
}

func readMatrix(w *mp4Walker, n *boxNode) (int, bool, error) {
	var b mp4.Tkhd
	if n.typ == "mvhd" {
		var mv mp4.Mvhd
		if err := w.unmarshal(n, &mv); err != nil {
			return 0, false, err
		}
		return rotationFromMatrix(mv.Matrix)
	}
	if err := w.unmarshal(n, &b); err != nil {
		return 0, false, err
	}
	return rotationFromMatrix(b.Matrix)
}

// fingerprint hashes the structural header plus a prefix of the essence, so
// checkpoints detect a different input without reading gigabytes.
func fingerprint(src media.Source, c *media.Container) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|", c.Format, c.Size)
	const head = 64 << 10
	buf := make([]byte, head)
	n, err := src.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return "", media.NewError(media.ClassIO, "cannot read input for fingerprint").
			WithCause(err)
	}
	h.Write(buf[:n])
	var counts []byte
	for _, t := range c.Tracks {
		counts = binary.BigEndian.AppendUint32(counts, t.ID.Value)
		counts = binary.BigEndian.AppendUint32(counts, uint32(t.SampleCount()))
	}
	h.Write(counts)
	return hex.EncodeToString(h.Sum(nil)), nil
}
