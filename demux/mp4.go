// Package demux incrementally parses MP4/MOV containers into the unified
// sample model. Parsing is offset-driven (io.ReaderAt) so sample payloads
// stay on disk until a consumer asks for them, bounding memory use.
package demux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"bcast/media"

	mp4 "github.com/abema/go-mp4"
)

// boxHdr is a parsed box header.
type boxHdr struct {
	typ   string
	off   int64 // absolute offset of the box header
	size  int64 // total size including header
	hsize int64 // header size (8 or 16)
}

func (h boxHdr) payloadOff() int64  { return h.off + h.hsize }
func (h boxHdr) payloadSize() int64 { return h.size - h.hsize }

// classifyKind maps a classified parse error to the shared issue taxonomy.
// trackID is -1 when the fault is not tied to a track (0 means unknown too,
// since track IDs are positive).
func classifyKind(err error, trackID int) media.IssueKind {
	switch {
	case errors.Is(err, media.ErrTruncated):
		return media.IssueTruncated
	case errors.Is(err, media.ErrBrokenBox):
		return media.IssueBrokenBox
	case errors.Is(err, media.ErrMissingMoov):
		return media.IssueMissingMoov
	case errors.Is(err, media.ErrIncompleteIndex):
		return media.IssueIncompleteIndex
	}
	return media.IssueBrokenBox
}

// readBoxHdr reads one box header at off. A box whose declared size runs
// past end is a truncation when it is media data, a broken box otherwise.
func readBoxHdr(r io.ReaderAt, off, end int64) (boxHdr, error) {
	var b [16]byte
	if off+8 > end {
		return boxHdr{}, media.Recoverable("demux.parse", off, media.ErrTruncated)
	}
	if _, err := r.ReadAt(b[:8], off); err != nil {
		return boxHdr{}, media.Recoverable("demux.parse", off, media.ErrTruncated)
	}
	size := int64(binary.BigEndian.Uint32(b[0:4]))
	h := boxHdr{typ: string(b[4:8]), off: off, size: size, hsize: 8}
	switch size {
	case 1:
		if off+16 > end {
			return boxHdr{}, media.Recoverable("demux.parse", off, media.ErrTruncated)
		}
		if _, err := r.ReadAt(b[8:16], off+8); err != nil {
			return boxHdr{}, media.Recoverable("demux.parse", off, media.ErrTruncated)
		}
		h.size = int64(binary.BigEndian.Uint64(b[8:16]))
		h.hsize = 16
	case 0:
		h.size = end - off
	}
	if h.size < h.hsize {
		return boxHdr{}, media.Recoverable("demux.parse", off,
			fmt.Errorf("%w: %q declares size %d", media.ErrBrokenBox, h.typ, h.size))
	}
	if off+h.size > end {
		if h.typ == "mdat" { // truncated media tail: clamp, samples re-checked later
			h.size = end - off
			return h, nil
		}
		return boxHdr{}, media.Recoverable("demux.parse", off,
			fmt.Errorf("%w: %q overruns container", media.ErrBrokenBox, h.typ))
	}
	return h, nil
}

// rawTrack holds the sample-table boxes of one trak.
type rawTrack struct {
	id        int
	kind      media.TrackKind
	kindSet   bool
	timescale uint32
	duration  int64
	stts      *mp4.Stts
	stsz      *mp4.Stsz
	stsc      *mp4.Stsc
	stco      *mp4.Stco
	co64      *mp4.Co64
	stss      *mp4.Stss
	ctts      *mp4.Ctts
	stsd      []byte // raw stsd box payload, preserved for remux
}

// File is a demuxed input: the shared media.File view plus the raw sample
// tables needed to expand sample entries.
type File struct {
	media.File
	raw []*rawTrack
}

// Parse incrementally demuxes an MP4/MOV of size bytes from r. On
// recoverable damage it returns the salvaged File plus a classified error;
// callers may proceed with the partial result.
func Parse(r io.ReaderAt, size int64, path string) (*File, error) {
	f := &File{}
	f.Path = path
	var firstErr error
	haveMoov := false
	for off := int64(0); off < size; {
		h, err := readBoxHdr(r, off, size)
		if err != nil {
			f.Damaged = append(f.Damaged, media.Range{Start: off, End: size, Reason: err.Error()})
			f.AddIssue(media.Issue{Kind: classifyKind(err, 0), Offset: off, TrackID: -1, Detail: err.Error()})
			firstErr = err
			break
		}
		if h.typ == "moov" {
			haveMoov = true
			if err := parseMoov(r, h, f); err != nil {
				return f, err
			}
		}
		off = h.off + h.size
	}
	if !haveMoov {
		f.AddIssue(media.Issue{Kind: media.IssueMissingMoov, Offset: -1, TrackID: -1, Detail: "no moov box"})
		return f, media.Fatal("demux.parse", media.ErrMissingMoov)
	}
	if err := buildAllSamples(f, size); err != nil && firstErr == nil {
		firstErr = err
	}
	return f, firstErr
}

// parseMoov walks moov children; each trak becomes a rawTrack plus a
// media.Track appended to f.Tracks.
func parseMoov(r io.ReaderAt, moov boxHdr, f *File) error {
	return walkChildren(r, moov, func(h boxHdr) error {
		if h.typ != "trak" {
			return nil
		}
		rt := &rawTrack{}
		err := walkChildren(r, h, func(c boxHdr) error {
			switch c.typ {
			case "tkhd":
				var tk mp4.Tkhd
				if err := unmarshalBox(r, c, &tk); err != nil {
					return err
				}
				rt.id = int(tk.TrackID)
			case "mdia":
				return parseMdia(r, c, rt, f)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if !rt.kindSet {
			return nil // unsupported or unidentifiable track kind: skipped
		}
		f.Tracks = append(f.Tracks, media.Track{
			ID: rt.id, Kind: rt.kind, Timescale: rt.timescale, Duration: rt.duration,
			CodecEntry: rt.stsd,
		})
		f.raw = append(f.raw, rt)
		return nil
	})
}

// parseMdia handles mdhd/hdlr directly and descends into minf/stbl.
func parseMdia(r io.ReaderAt, mdia boxHdr, rt *rawTrack, f *File) error {
	return walkChildren(r, mdia, func(c boxHdr) error {
		switch c.typ {
		case "mdhd":
			var md mp4.Mdhd
			if err := unmarshalBox(r, c, &md); err != nil {
				return err
			}
			rt.timescale = md.Timescale
			rt.duration = int64(md.DurationV0)
			if md.GetVersion() == 1 {
				rt.duration = int64(md.DurationV1)
			}
		case "hdlr":
			var hd mp4.Hdlr
			if err := unmarshalBox(r, c, &hd); err != nil {
				return err
			}
			switch string(hd.HandlerType[:]) {
			case "vide":
				rt.kind, rt.kindSet = media.TrackVideo, true
			case "soun":
				rt.kind, rt.kindSet = media.TrackAudio, true
			default:
				rt.kindSet = false // unsupported track kind: skipped
				f.AddIssue(media.Issue{
					Kind: media.IssueUnsupportedTrak, Offset: c.off, TrackID: rt.id,
					Detail: fmt.Sprintf("handler %q not audio/video", hd.HandlerType[:]),
				})
			}
		case "minf":
			return walkChildren(r, c, func(stbl boxHdr) error {
				if stbl.typ != "stbl" {
					return nil
				}
				return parseStbl(r, stbl, rt)
			})
		}
		return nil
	})
}

// parseStbl unmarshals the sample-table leaf boxes into rt.
func parseStbl(r io.ReaderAt, stbl boxHdr, rt *rawTrack) error {
	return walkChildren(r, stbl, func(h boxHdr) error {
		var err error
		switch h.typ {
		case "stts":
			rt.stts = new(mp4.Stts)
			err = unmarshalBox(r, h, rt.stts)
		case "stsz":
			rt.stsz = new(mp4.Stsz)
			err = unmarshalBox(r, h, rt.stsz)
		case "stsc":
			rt.stsc = new(mp4.Stsc)
			err = unmarshalBox(r, h, rt.stsc)
		case "stco":
			rt.stco = new(mp4.Stco)
			err = unmarshalBox(r, h, rt.stco)
		case "co64":
			rt.co64 = new(mp4.Co64)
			err = unmarshalBox(r, h, rt.co64)
		case "stss":
			rt.stss = new(mp4.Stss)
			err = unmarshalBox(r, h, rt.stss)
		case "ctts":
			rt.ctts = new(mp4.Ctts)
			err = unmarshalBox(r, h, rt.ctts)
		case "stsd":
			rt.stsd, err = readSampleEntry(r, h)
		}
		if err != nil {
			return media.Recoverable("demux.stbl", h.off,
				fmt.Errorf("%w: %s", media.ErrBrokenBox, h.typ))
		}
		return nil
	})
}

// walkChildren iterates the child boxes of a container box.
func walkChildren(r io.ReaderAt, parent boxHdr, fn func(h boxHdr) error) error {
	for off := parent.payloadOff(); off < parent.off+parent.size; {
		h, err := readBoxHdr(r, off, parent.off+parent.size)
		if err != nil {
			return err
		}
		if err := fn(h); err != nil {
			return err
		}
		off = h.off + h.size
	}
	return nil
}

// unmarshalBox decodes one leaf box payload with go-mp4.
func unmarshalBox(r io.ReaderAt, h boxHdr, dst mp4.IBox) error {
	sr := io.NewSectionReader(r, h.payloadOff(), h.payloadSize())
	_, err := mp4.Unmarshal(sr, uint64(h.payloadSize()), dst, mp4.Context{})
	return err
}

// readBoxBytes copies a whole box (header included) so it can be re-emitted
// byte-for-byte in a remuxed file.
func readBoxBytes(r io.ReaderAt, h boxHdr) ([]byte, error) {
	buf := make([]byte, h.size)
	if _, err := r.ReadAt(buf, h.off); err != nil {
		return nil, err
	}
	return buf, nil
}

// readSampleEntry returns the first sample-entry box inside stsd (header
// included), with the stsd container itself stripped. Callers rebuild the
// stsd framing, so storing the child keeps the bytes comparable across a
// remux. A truncated or empty stsd yields no entry rather than an error:
// codec configuration is optional for the timeline work.
func readSampleEntry(r io.ReaderAt, stsd boxHdr) ([]byte, error) {
	// FullBox(4) + EntryCount(4), then the entries.
	first := stsd.payloadOff() + 8
	end := stsd.off + stsd.size
	if first+8 > end {
		return nil, nil
	}
	h, err := readBoxHdr(r, first, end)
	if err != nil {
		return nil, nil // malformed stsd: keep parsing sample tables
	}
	return readBoxBytes(r, h)
}
