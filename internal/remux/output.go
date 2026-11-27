// Package remux writes an ordered packet stream into a target container
// without decoding or re-encoding a single byte.
//
// Every output stream's codec parameters are copied from the source, and each
// packet's encoded payload is written verbatim. Only timestamps are rescaled
// from the source time base into the output stream's time base, which is what
// the container format itself requires; this is metadata, not sample data.
//
// When the target container would require rewriting samples (for example
// changing the codec tag, re-packaging a bitstream format the muxer cannot
// copy, or resizing the coded frame), the writer refuses before it writes any
// output, rather than emitting a file whose packets no longer match the
// declared codec.
package remux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/asticode/go-astiav"

	"avpipe/internal/media"
)

// ErrCanceled is returned when writing is canceled.
var ErrCanceled = errors.New("remux: canceled")

// TrackSpec is one source track to include in the output.
type TrackSpec struct {
	Track media.Track
	// TimeBaseOverride, when valid, replaces the source time base for the
	// output stream. Zero means the demuxer's time base is used.
	TimeBaseOverride media.Rational
}

// Config describes an output.
type Config struct {
	// Format is the libavformat muxer name, e.g. "mp4" or "mxf".
	Format string
	// Path is the destination file. Its parent must exist.
	Path string
	// Tracks are written in the given order; stream indices follow it.
	Tracks []TrackSpec
	// Metadata is copied onto the output container.
	Metadata map[string]string
}

// Writer is an open output container.
type Writer struct {
	cfg      Config
	fc       *astiav.FormatContext
	io       *astiav.IOContext
	streams  map[string]*outStream
	order    []string
	written   int64
	bytes     int64
	finalized bool
	canceled  bool
	started   bool
	// tmpPath is the temporary file written before the atomic rename.
	tmpPath string
}

type outStream struct {
	in     media.Track
	out    *astiav.Stream
	base   media.Rational
	params bool // codec parameters installed
}

// Open validates the configuration and opens the temporary output. Nothing is
// published at Path until Commit succeeds.
func Open(ctx context.Context, cfg Config) (*Writer, error) {
	if cfg.Format == "" {
		return nil, media.Errorf(media.KindUsage, media.Origin{}, "remux.Open", "output format is required")
	}
	if cfg.Path == "" {
		return nil, media.Errorf(media.KindUsage, media.Origin{}, "remux.Open", "output path is required")
	}
	if len(cfg.Tracks) == 0 {
		return nil, media.Errorf(media.KindUsage, media.Origin{File: cfg.Path}, "remux.Open", "at least one track is required")
	}
	out := astiav.FindOutputFormat(cfg.Format)
	if out == nil {
		return nil, media.Errorf(media.KindUsage, media.Origin{File: cfg.Path}, "remux.Open",
			"unknown output format %q", cfg.Format)
	}
	if err := ctx.Err(); err != nil {
		return nil, media.New(media.KindCancel, media.Origin{File: cfg.Path}, "remux.Open", "canceled before opening", err)
	}
	// Write into a sibling temp file so a failed or canceled run never leaves a
	// half-written product at the published path.
	dir := filepath.Dir(cfg.Path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(cfg.Path)+".tmp-*")
	if err != nil {
		return nil, media.New(media.KindUsage, media.Origin{File: cfg.Path}, "remux.Open", "creating temp file failed", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()

	fc, err := astiav.AllocOutputFormatContext(nil, cfg.Format, tmpPath)
	if err != nil || fc == nil {
		os.Remove(tmpPath)
		return nil, media.New(media.KindCorrupt, media.Origin{File: cfg.Path}, "remux.Open",
			"allocating output context failed", err)
	}
	w := &Writer{cfg: cfg, fc: fc, streams: map[string]*outStream{}, tmpPath: tmpPath}
	if err := w.addStreams(ctx); err != nil {
		w.abort()
		return nil, err
	}
	// Open the file unless the muxer is headerless.
	if !fc.OutputFormat().Flags().Has(astiav.IOFormatFlagNofile) {
		io, err := astiav.OpenIOContext(tmpPath, astiav.NewIOContextFlags(astiav.IOContextFlagWrite), nil, nil)
		if err != nil {
			w.abort()
			return nil, media.New(media.KindCorrupt, media.Origin{File: cfg.Path}, "remux.Open",
				"opening output file failed", err)
		}
		w.io = io
		fc.SetPb(io)
	}
	// The container header is written by Start, after every stream's codec
	// parameters have been installed, because a muxer validates them there.
	return w, nil
}

// Start writes the container header. It must be called after every output
// stream's codec parameters have been installed with AcceptParameters and
// before the first WritePacket.
func (w *Writer) Start(ctx context.Context) error {
	if w.started {
		return media.Errorf(media.KindInternal, media.Origin{File: w.cfg.Path}, "remux.Start", "already started")
	}
	if err := ctx.Err(); err != nil {
		w.abort()
		return media.New(media.KindCancel, media.Origin{File: w.cfg.Path}, "remux.Start", "canceled", err)
	}
	for id, s := range w.streams {
		if !s.params {
			w.abort()
			return media.Errorf(media.KindPolicy, media.Origin{File: w.cfg.Path, TrackID: id},
				"remux.Start", "track %q has no codec parameters", id)
		}
	}
	if err := w.fc.WriteHeader(nil); err != nil {
		w.abort()
		return media.New(media.KindCorrupt, media.Origin{File: w.cfg.Path}, "remux.Start",
			"writing container header failed (target cannot accept the copied streams)", err)
	}
	w.started = true
	return nil
}

// addStreams copies codec parameters and validates that the target can carry
// the packets without sample rewriting.
func (w *Writer) addStreams(ctx context.Context) error {
	for _, spec := range w.cfg.Tracks {
		if err := ctx.Err(); err != nil {
			return media.New(media.KindCancel, media.Origin{File: w.cfg.Path}, "remux.addStreams", "canceled", err)
		}
		t := spec.Track
		if t.ID == "" || !t.TimeBase.Valid() {
			return media.Errorf(media.KindPolicy, media.Origin{File: w.cfg.Path, TrackID: t.ID},
				"remux.addStreams", "track %q has no stable ID or time base", t.ID)
		}
		if _, dup := w.streams[t.ID]; dup {
			return media.Errorf(media.KindPolicy, media.Origin{File: w.cfg.Path, TrackID: t.ID},
				"remux.addStreams", "duplicate output track %q", t.ID)
		}
		if t.Display.Invalid {
			return media.Errorf(media.KindPolicy, media.Origin{File: w.cfg.Path, TrackID: t.ID},
				"remux.addStreams", "refusing to write invalid track %q: %s", t.ID, t.Display.InvalidReason)
		}
		// The muxer must be able to accept this codec as-is.
		if err := checkCodecSupported(w.cfg.Format, t); err != nil {
			return err
		}
		os := w.fc.NewStream(nil)
		if os == nil {
			return media.Errorf(media.KindInternal, media.Origin{File: w.cfg.Path, TrackID: t.ID},
				"remux.addStreams", "allocating output stream failed")
		}
		// Copy codec parameters from the demuxed track is done by the caller
		// via CopyParameters before writing; here we only record the mapping.
		base := t.TimeBase
		if spec.TimeBaseOverride.Valid() {
			base = spec.TimeBaseOverride
		}
		os.SetTimeBase(rationalTo(base))
		os.SetID(len(w.order) + 1)
		w.streams[t.ID] = &outStream{in: t, out: os, base: base}
		w.order = append(w.order, t.ID)
	}
	return nil
}

// AcceptParameters installs the source codec parameters onto the matching
// output stream. Callers obtain parameters from the demuxer.
func (w *Writer) AcceptParameters(trackID string, cp *astiav.CodecParameters) error {
	s, ok := w.streams[trackID]
	if !ok {
		return media.Errorf(media.KindPolicy, media.Origin{File: w.cfg.Path, TrackID: trackID},
			"remux.AcceptParameters", "unknown track %q", trackID)
	}
	if cp == nil {
		return media.Errorf(media.KindInternal, media.Origin{File: w.cfg.Path, TrackID: trackID},
			"remux.AcceptParameters", "nil codec parameters")
	}
	if err := cp.Copy(s.out.CodecParameters()); err != nil {
		return media.New(media.KindCorrupt, media.Origin{File: w.cfg.Path, TrackID: trackID},
			"remux.AcceptParameters", "copying codec parameters failed", err)
	}
	// A zero codec tag lets the muxer choose its native tag; forcing the source
	// tag could make it re-time or re-tag samples.
	s.out.CodecParameters().SetCodecTag(0)
	s.params = true
	return nil
}

// WritePacket writes one encoded packet verbatim. data is the encoded payload;
// its bytes are not modified. Timestamps are rescaled between the source and
// target time bases.
func (w *Writer) WritePacket(p media.Packet) error {
	if w.finalized {
		return media.Errorf(media.KindInternal, p.Origin, "remux.WritePacket", "writer already finalized")
	}
	if !w.started {
		return media.Errorf(media.KindInternal, p.Origin, "remux.WritePacket", "Start not called")
	}
	s, ok := w.streams[p.TrackID]
	if !ok {
		return media.Errorf(media.KindPolicy, p.Origin, "remux.WritePacket", "unmapped track %q", p.TrackID)
	}
	idx := s.out.Index()
	pts, err := rescaleTimestamp(p.PTS, p.TimeBase, s.base)
	if err != nil {
		return withOrigin(err, p.Origin)
	}
	dstPts := pts
	dstDts := astiav.NoPtsValue
	if p.HasDTS() {
		dts, err := rescaleTimestamp(p.DTS, p.TimeBase, s.base)
		if err != nil {
			return withOrigin(err, p.Origin)
		}
		dstDts = dts
	}
	opkt := astiav.AllocPacket()
	defer opkt.Free()
	if err := opkt.FromData(p.Data); err != nil {
		return media.New(media.KindInternal, p.Origin, "remux.WritePacket", "copying payload failed", err)
	}
	opkt.SetStreamIndex(idx)
	opkt.SetPts(dstPts)
	if dstDts != astiav.NoPtsValue {
		opkt.SetDts(dstDts)
	}
	opkt.SetDuration(p.Duration)
	opkt.SetPos(-1)
	var flags astiav.PacketFlags
	if p.Key {
		flags = flags.Add(astiav.PacketFlagKey)
	}
	opkt.SetFlags(flags)
	if err := w.fc.WriteInterleavedFrame(opkt); err != nil {
		return media.New(media.KindCorrupt, p.Origin, "remux.WritePacket",
			"writing packet failed", err)
	}
	w.written++
	w.bytes += int64(len(p.Data))
	return nil
}

func rescaleTimestamp(ts int64, src, dst media.Rational) (int64, error) {
	if ts == media.NoTimestamp {
		return astiav.NoPtsValue, nil
	}
	if !src.Valid() || !dst.Valid() {
		return 0, media.Errorf(media.KindInternal, media.Origin{}, "remux.rescaleTimestamp",
			"invalid time bases %s -> %s", src, dst)
	}
	if src.Equal(dst) {
		return ts, nil
	}
	v, err := media.Rescale(ts, src, dst)
	if err != nil {
		return 0, media.New(media.KindInternal, media.Origin{}, "remux.rescaleTimestamp", "rescale failed", err)
	}
	return v, nil
}

// withOrigin attaches a packet origin to an error when it is a *media.Error.
func withOrigin(err error, o media.Origin) error {
	if me, ok := media.AsError(err); ok {
		return me.WithOrigin(o)
	}
	return media.New(media.KindInternal, o, "remux", "timestamp handling failed", err)
}

// Commit flushes the trailer and atomically renames the temp file into place.
// After Commit the writer is finalized. On error nothing is published.
func (w *Writer) Commit(ctx context.Context) error {
	if w.finalized {
		return media.Errorf(media.KindInternal, media.Origin{File: w.cfg.Path}, "remux.Commit", "already finalized")
	}
	if err := ctx.Err(); err != nil {
		w.abort()
		return media.New(media.KindCancel, media.Origin{File: w.cfg.Path}, "remux.Commit", "canceled before commit", err)
	}
	if err := w.fc.WriteTrailer(); err != nil {
		w.abort()
		return media.New(media.KindCorrupt, media.Origin{File: w.cfg.Path}, "remux.Commit",
			"writing container trailer failed", err)
	}
	if w.io != nil {
		if err := w.io.Close(); err != nil {
			w.abort()
			return media.New(media.KindCorrupt, media.Origin{File: w.cfg.Path}, "remux.Commit",
				"closing output failed", err)
		}
		w.io = nil
	}
	w.fc.Free()
	w.fc = nil
	// Publish atomically. A reader either sees the complete file or nothing.
	if err := os.Rename(w.tmpPath, w.cfg.Path); err != nil {
		os.Remove(w.tmpPath)
		return media.New(media.KindInternal, media.Origin{File: w.cfg.Path}, "remux.Commit",
			"publishing output failed", err)
	}
	w.tmpPath = ""
	w.finalized = true
	return nil
}

// Abort discards the temporary output without publishing anything.
func (w *Writer) Abort() { w.abort() }

func (w *Writer) abort() {
	if w.io != nil {
		w.io.Close() //nolint:errcheck
		w.io = nil
	}
	if w.fc != nil {
		w.fc.Free()
		w.fc = nil
	}
	if w.tmpPath != "" {
		os.Remove(w.tmpPath)
		w.tmpPath = ""
	}
	w.finalized = true
}

// Stats reports how much was written.
func (w *Writer) Stats() (packets int64, bytes int64) { return w.written, w.bytes }

// Finalized reports whether Commit or Abort ran.
func (w *Writer) Finalized() bool { return w.finalized }

func rationalTo(r media.Rational) astiav.Rational {
	return astiav.NewRational(int(r.Num), int(r.Den))
}

// checkCodecSupported rejects containers/codec combinations that would force
// the muxer to rewrite samples instead of copying them.
func checkCodecSupported(format string, t media.Track) error {
	codec := strings.ToLower(t.Codec)
	switch format {
	case "mxf":
		if t.Kind == media.TrackAudio {
			switch codec {
			case "pcm_s16le", "pcm_s24le", "pcm_s16be":
				return nil
			default:
				return media.Errorf(media.KindCorrupt, media.Origin{TrackID: t.ID},
					"remux.checkCodecSupported",
					"mxf cannot copy %s audio without rewriting samples", codec)
			}
		}
		switch codec {
		case "mpeg2video", "h264", "dnxhd", "jpeg2000":
			return nil
		default:
			return media.Errorf(media.KindCorrupt, media.Origin{TrackID: t.ID},
				"remux.checkCodecSupported",
				"mxf cannot copy %s video without rewriting samples", codec)
		}
	case "mp4", "mov":
		switch codec {
		case "h264", "h265", "hevc", "mpeg4", "aac", "mp3", "opus", "flac", "alac":
			return nil
		case "mpeg2video":
			return media.Errorf(media.KindCorrupt, media.Origin{TrackID: t.ID},
				"remux.checkCodecSupported",
				"mp4 cannot copy mpeg2video without rewriting samples")
		case "pcm_s16le", "pcm_s24le":
			return media.Errorf(media.KindCorrupt, media.Origin{TrackID: t.ID},
				"remux.checkCodecSupported",
				"mp4 cannot copy raw PCM without rewriting samples")
		default:
			return media.Errorf(media.KindCorrupt, media.Origin{TrackID: t.ID},
				"remux.checkCodecSupported", "mp4 cannot copy %s without rewriting samples", codec)
		}
	default:
		return media.Errorf(media.KindUsage, media.Origin{},
			"remux.checkCodecSupported", "unsupported output format %q", format)
	}
}

var _ = fmt.Sprintf
