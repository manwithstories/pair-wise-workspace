package ingest

import (
	"fmt"
	"io"
	"os"

	"github.com/abema/go-mp4"

	"mediarun/internal/mediaerr"
	"mediarun/internal/timebase"
)

// mp4Streamer parses the documented non-fragmented ISOBMFF subset:
// ftyp/moov/trak/mdia(mdhd,hdlr,minf/stbl(stsd,stts,ctts,stsc,stsz,stco|co64))
// and streams mdat samples. Box boundaries are validated by boxWalker, so
// truncation is reported with the exact offset rather than tolerated.
type mp4Streamer struct {
	path   string
	file   *os.File
	size   int64
	info   ContainerInfo
	tracks []*mp4Track
	cursor int // next track index in the merged, file-order queue
	idx    []int64
}

type mp4Track struct {
	desc     TrackDesc
	tb       timebase.Timebase
	samples  []flatSample // fully resolved sample table
	next     int
	dtsPrev  int64
	hasDTS   bool
}

type flatSample struct {
	offset   int64
	size     int64
	dts      int64
	pts      int64
	duration int64
}

// OpenMP4 opens and parses the header tables of an MP4 input.
func OpenMP4(path string) (Streamer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(path)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(path)
	}
	s := &mp4Streamer{path: path, file: f, size: st.Size()}
	s.info = ContainerInfo{Format: "mp4", Path: path, Size: st.Size()}
	if err := s.parseMoov(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return s, nil
}

func (s *mp4Streamer) Info() ContainerInfo { return s.info }

func (s *mp4Streamer) Close() error { return s.file.Close() }

// parseMoov locates moov and each trak through boundary-checked walks.
func (s *mp4Streamer) parseMoov() error {
	w := newBoxWalker(s.file, s.size)
	var moov *boxHeader
	for {
		b, err := w.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return onPath(err, s.path)
		}
		if boxTypeEqual(b.Type, mp4.BoxTypeMoov()) {
			moov = b
			break
		}
	}
	if moov == nil {
		return mediaerr.New(mediaerr.CodeUnknownStructure, "missing moov box").
			At("mp4", "", 0).OnPath(s.path)
	}
	return findChild(s.file, moov, mp4.BoxTypeTrak(), func(trak *boxHeader) (bool, error) {
		tr, err := s.parseTrak(trak)
		if err != nil {
			return false, err
		}
		s.tracks = append(s.tracks, tr)
		s.info.Tracks = append(s.info.Tracks, tr.desc)
		return false, nil
	})
}

func (s *mp4Streamer) parseTrak(trak *boxHeader) (*mp4Track, error) {
	tr := &mp4Track{}
	var mdia *boxHeader
	if err := findChild(s.file, trak, mp4.BoxTypeMdia(), func(b *boxHeader) (bool, error) {
		mdia = b
		return true, nil
	}); err != nil {
		return nil, err
	}
	if mdia == nil {
		return nil, mediaerr.New(mediaerr.CodeUnknownStructure, "trak has no mdia").
			At("mp4", "", trak.HeaderStart).OnPath(s.path)
	}
	var scale int64
	var kind TrackKind
	var codec string
	var width, height uint32
	var channels uint16
	if err := findChild(s.file, mdia, mp4.BoxTypeMdhd(), func(b *boxHeader) (bool, error) {
		v, err := parseBox(s.file, b)
		if err != nil {
			return true, err
		}
		scale = int64(v.(*mp4.Mdhd).Timescale)
		return true, nil
	}); err != nil {
		return nil, err
	}
	if err := findChild(s.file, mdia, mp4.BoxTypeHdlr(), func(b *boxHeader) (bool, error) {
		v, err := parseBox(s.file, b)
		if err != nil {
			return true, err
		}
		switch string(v.(*mp4.Hdlr).HandlerType[:]) {
		case "vide":
			kind = KindVideo
		case "soun":
			kind = KindAudio
		default:
			kind = TrackKind(string(v.(*mp4.Hdlr).HandlerType[:]))
		}
		return true, nil
	}); err != nil {
		return nil, err
	}
	var stbl *boxHeader
	if err := findChild(s.file, mdia, mp4.BoxTypeMinf(), func(minf *boxHeader) (bool, error) {
		return true, findChild(s.file, minf, mp4.BoxTypeStbl(), func(b *boxHeader) (bool, error) {
			stbl = b
			return true, nil
		})
	}); err != nil {
		return nil, err
	}
	if stbl == nil {
		return nil, mediaerr.New(mediaerr.CodeUnknownStructure, "mdia has no stbl").
			At("mp4", "", mdia.HeaderStart).OnPath(s.path)
	}
	codec, width, height, channels = s.parseStsd(stbl)
	tr.tb = timebase.NewTimebase(scale)
	tr.desc = TrackDesc{
		ID: fmt.Sprintf("%s/%d", kind, len(s.tracks)), Kind: kind, Codec: codec,
		Timescale: scale, Width: width, Height: height, Channels: channels,
	}
	var perr error
	if err := findChild(s.file, stbl, mp4.BoxTypeStts(), func(b *boxHeader) (bool, error) {
		perr = tr.loadTiming(s.file, b)
		return true, perr
	}); err != nil {
		return nil, err
	}
	if perr != nil {
		return nil, perr
	}
	if err := findChild(s.file, stbl, mp4.BoxTypeCtts(), func(b *boxHeader) (bool, error) {
		perr = tr.applyCtts(s.file, b)
		return true, perr
	}); err != nil {
		return nil, err
	}
	if perr != nil {
		return nil, perr
	}
	if err := tr.loadChunks(s.file, stbl); err != nil {
		return nil, err
	}
	tr.desc.SampleCount = int64(len(tr.samples))
	return tr, nil
}
