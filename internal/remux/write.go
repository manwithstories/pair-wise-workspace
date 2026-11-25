// Package remux writes validated samples into a new MP4 container without any
// decoding or re-encoding: payload bytes are copied verbatim and the output
// sample table re-indexes the same packets. Outputs and manifests land in a
// temporary file and are atomically renamed only after a successful close.
package remux

import (
	"bytes"
	"os"
	"path/filepath"

	"mediaarchive/internal/clock"
	"mediaarchive/internal/demux"
	"mediaarchive/internal/model"
)

// Writer buffers validated packets and emits one self-describing container at
// Close. Buffering keeps every sample offset computable before any byte is
// written, so the armd table and mdat always agree.
type Writer struct {
	dst     string
	tmp     *os.File
	tmpPath string
	samples []clock.NormalizedSample
	stats   Stats
	damages []Damage
	brands  []string
}

// NewWriter creates a temp file in the destination directory.
func NewWriter(dst string, brands []string) (*Writer, error) {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, model.NewError(model.StageRemux, "cannot create output dir").Wrap(err)
	}
	tmp, err := os.CreateTemp(dir, ".remux-*")
	if err != nil {
		return nil, model.NewError(model.StageRemux, "cannot create temp output").Wrap(err)
	}
	return &Writer{
		dst:     dst,
		tmp:     tmp,
		tmpPath: tmp.Name(),
		brands:  brands,
		stats:   Stats{PacketHash: map[uint32][][32]byte{}},
	}, nil
}

// AddSample buffers one packet. Truncated samples are dropped as damage;
// every kept payload is byte-copied, never re-encoded.
func (wr *Writer) AddSample(s clock.NormalizedSample) error {
	wr.stats.Packets++
	if s.Flags&demux.FlagTruncated != 0 {
		wr.stats.Dropped++
		return nil
	}
	wr.samples = append(wr.samples, s)
	wr.stats.PacketHash[s.TrackID] = append(wr.stats.PacketHash[s.TrackID], s.SHA256)
	wr.stats.PacketsKept++
	wr.stats.PayloadBytes += int64(len(s.Payload))
	return nil
}

// AddDamage records a recovered event from demux/RTP stages.
func (wr *Writer) AddDamage(d Damage) { wr.damages = append(wr.damages, d) }

// Close writes ftyp, moov(armd), mdat in one pass and atomically replaces
// the destination. Every sample offset is resolved from the header sizes
// before any byte is committed, so index and data can never disagree.
func (wr *Writer) Close(tracks []demux.TrackMeta) (*Stats, error) {
	ftyp := buildFtyp(wr.brands)
	// First armd build uses placeholder offsets; its serialized length is
	// fixed regardless of offset values (all offsets are 8 bytes), so the
	// mdat position derived from it is exact.
	armd0, perChunk, err := buildArmd(tracks, wr.samples, wr.damages, 0)
	if err != nil {
		return nil, err
	}
	moov0, err := wrapBox("moov", wrapRaw("armd", armd0))
	if err != nil {
		return nil, err
	}
	mdatHeader := 8
	mdatPos := int64(len(ftyp) + len(moov0) + mdatHeader)
	armd, err := finalizeOffsets(armd0, perChunk, mdatPos)
	if err != nil {
		return nil, err
	}
	moov, err := wrapBox("moov", wrapRaw("armd", armd))
	if err != nil {
		return nil, err
	}
	var mdatBody bytes.Buffer
	for _, s := range wr.samples {
		mdatBody.Write(s.Payload)
	}
	mdat := wrapRaw("mdat", mdatBody.Bytes())
	for _, chunk := range [][]byte{ftyp, moov, mdat} {
		if _, err := wr.tmp.Write(chunk); err != nil {
			return nil, model.NewError(model.StageRemux, "container write failed").Wrap(err)
		}
	}
	if err := wr.tmp.Sync(); err != nil {
		return nil, model.NewError(model.StageRemux, "fsync failed").Wrap(err)
	}
	if err := wr.tmp.Close(); err != nil {
		return nil, model.NewError(model.StageRemux, "close temp failed").Wrap(err)
	}
	if err := os.Rename(wr.tmpPath, wr.dst); err != nil {
		return nil, model.NewError(model.StageRemux, "atomic rename failed").Wrap(err)
	}
	wr.stats.Damages = wr.damages
	st := wr.stats
	return &st, nil
}

// Abort removes the temp file after cancellation or a hard failure.
func (wr *Writer) Abort() {
	if wr.tmp != nil {
		wr.tmp.Close()
	}
	if wr.tmpPath != "" {
		os.Remove(wr.tmpPath)
	}
}
