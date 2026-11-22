// MP4/MOV audio container adapter: entry point and root-level walk.
//
// The adapter parses structure only. Media bytes stay on disk and are read
// on demand through Source.Reader/Cursor; memory is bounded by one sample
// plus the sample tables. Track-level parsing lives in mp4_track.go and the
// sample-table expansion in mp4_table.go.
package container

import (
	"io"
	"os"

	"github.com/abema/go-mp4"

	"mediacore/internal/media"
)

// MP4Version is the adapter revision bound into checkpoints.
var MP4Version = Version{Name: "mp4", Major: 1, Minor: 0}

// MP4 is the versioned MP4/MOV audio adapter.
type MP4 struct {
	Opts Options
}

// NewMP4 returns an adapter with the given resource bounds.
func NewMP4(opts Options) *MP4 {
	if opts.MaxMetadataBytes <= 0 {
		opts = DefaultOptions()
	}
	return &MP4{Opts: opts}
}

// Version implements the adapter contract.
func (a *MP4) Version() Version { return MP4Version }

// OpenFile parses path. The caller owns the returned source and file.
func (a *MP4) OpenFile(path string) (*Source, *os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, media.NewError(media.CodeUsage, -1, 0, "open %s: %v", path, err).Wrap(err)
	}
	src, err := a.Open(f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return src, f, nil
}

type parseState struct {
	opts     Options
	src      *Source
	fileSize int64
	info     Info
	mdats    []Range
	moovOff  int64
	seenFtyp bool
	seenMoov bool
}

// Open parses an MP4/MOV from r. r must remain readable for the lifetime of
// the returned Source.
func (a *MP4) Open(r io.ReaderAt) (*Source, error) {
	size, err := sizeOf(r)
	if err != nil {
		return nil, media.NewError(media.CodeUsage, -1, 0, "determine size: %v", err).Wrap(err)
	}
	st := &parseState{
		opts:     a.Opts,
		src:      &Source{r: r, opts: a.Opts, version: MP4Version},
		fileSize: size,
	}
	if err := st.walk(); err != nil {
		return nil, err
	}
	if !st.seenMoov {
		return nil, media.NewError(media.CodeMissingBox, 0, 0, "missing moov box")
	}
	if len(st.src.tracks) == 0 {
		return nil, media.NewError(media.CodeNoAudioTrack, -1, 0, "no audio track in moov")
	}
	st.info.FileSize = size
	st.info.MoovOffset = st.moovOff
	st.info.Mdats = st.mdats
	st.info.TrackCount = len(st.src.tracks)
	st.info.AudioTracks = len(st.src.tracks)
	st.info.DurationMS = st.durationMS()
	st.src.mdats = st.mdats
	st.src.info = st.info
	return st.src, nil
}

func (st *parseState) durationMS() int64 {
	var best int64
	for _, t := range st.src.tracks {
		if t.Timescale > 0 && t.Duration > 0 {
			ms, _, err := media.TimeBase{Timescale: t.Timescale}.Duration(t.Duration, media.TimeBase{Timescale: 1000})
			if err == nil && ms > best {
				best = ms
			}
		}
	}
	return best
}

// walk reads the root box sequence one header at a time. Any length that
// exceeds the file is rejected by readBoxHeader before the walk continues.
func (st *parseState) walk() error {
	off := int64(0)
	first := true
	for off < st.fileSize {
		bi, err := readBoxHeader(st.src.r, off, st.fileSize)
		if err != nil {
			return err
		}
		if first && bi.typ != "ftyp" && bi.typ != "moov" && bi.typ != "mdat" && bi.typ != "free" && bi.typ != "wide" {
			return media.NewError(media.CodeInvalidBoxLength, off, 0,
				"unexpected first box type %q", bi.typ)
		}
		first = false
		switch bi.typ {
		case "ftyp":
			if err := st.parseFtyp(bi); err != nil {
				return err
			}
			st.seenFtyp = true
		case "moov":
			if err := st.parseMoov(bi); err != nil {
				return err
			}
			st.seenMoov = true
			st.moovOff = bi.start
		case "mdat":
			st.mdats = append(st.mdats, Range{Start: bi.payload, End: bi.end})
		}
		if bi.end <= off {
			return media.NewError(media.CodeInvalidBoxLength, off, 0, "box %q does not advance", bi.typ)
		}
		off = bi.end
	}
	return nil
}

func (st *parseState) parseFtyp(bi boxHeader) error {
	if bi.size-bi.headerSize > st.opts.MaxMetadataBytes {
		return media.NewError(media.CodeOversizedBox, bi.start, 0, "ftyp too large")
	}
	body, err := readPayload(st.src.r, bi)
	if err != nil {
		return err
	}
	if len(body) < 8 {
		return media.NewError(media.CodeInvalidBoxLength, bi.start, 0, "ftyp payload %d < 8", len(body))
	}
	st.info.MajorBrand = string(body[0:4])
	for i := 8; i+4 <= len(body); i += 4 {
		st.info.Compatible = append(st.info.Compatible, string(body[i:i+4]))
	}
	return nil
}

func (st *parseState) parseMoov(bi boxHeader) error {
	if bi.size-bi.headerSize > st.opts.MaxMetadataBytes {
		return media.NewError(media.CodeOversizedBox, bi.start, 0, "moov too large")
	}
	return children(st.src.r, bi.payload, bi.end, func(child boxHeader) error {
		if child.typ == "trak" {
			return st.parseTrak(child)
		}
		return nil
	})
}

// decodeBox unmarshals a full box body into dst with go-mp4.
func (st *parseState) decodeBox(bi boxHeader, dst mp4.IBox) error {
	body, err := readPayload(st.src.r, bi)
	if err != nil {
		return err
	}
	if _, err := mp4.Unmarshal(bytesReader(body), uint64(len(body)), dst, mp4.Context{}); err != nil {
		return media.NewError(media.CodeInvalidBoxLength, bi.start, 0, "parse %s: %v", bi.typ, err).Wrap(err)
	}
	return nil
}
