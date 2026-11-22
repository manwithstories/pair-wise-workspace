package mp4

import (
	"fmt"
	"io"
	"os"

	gomp4 "github.com/abema/go-mp4"

	"mediabridge/internal/adapter"
	"mediabridge/internal/clock"
	"mediabridge/internal/fault"
	"mediabridge/internal/media"
)

// parser holds the state for a single asset demux.
type parser struct {
	src  adapter.Source
	opts adapter.Options
	file *os.File
	sr   *sampleReader
	rs   *offsetReader

	asset *media.Asset

	tables []tableEntry
}

// sampleTable is the expanded timing/size layout of one track.
type sampleTable struct {
	count     int64
	dts       []int64 // media timescale units, length count
	pts       []int64
	dur       []int64
	sizes     []uint32
	offsets   []int64
	sync      []bool
	syncKnown bool // false when no stss: every sample is a sync sample (audio)
}

func newParser(src adapter.Source, opts adapter.Options, f *os.File) *parser {
	if opts.MaxPacketsPerTrack <= 0 {
		opts.MaxPacketsPerTrack = clock.MaxPacketsPerTrack
	}
	if opts.MaxAssetDurationNS <= 0 {
		opts.MaxAssetDurationNS = clock.MaxAssetDurationNanos
	}
	return &parser{
		src:  src,
		opts: opts,
		file: f,
		sr:   &sampleReader{f: f},
	}
}

// offsetReader wraps the file for go-mp4, which requires io.ReadSeeker.
type offsetReader struct {
	f   *os.File
	pos int64
}

func (o *offsetReader) Read(p []byte) (int, error) {
	n, err := o.f.ReadAt(p, o.pos)
	o.pos += int64(n)
	return n, err
}

func (o *offsetReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		o.pos = offset
	case io.SeekCurrent:
		o.pos += offset
	case io.SeekEnd:
		st, err := o.f.Stat()
		if err != nil {
			return 0, err
		}
		o.pos = st.Size() + offset
	default:
		return 0, fmt.Errorf("mp4: invalid whence %d", whence)
	}
	return o.pos, nil
}

func (p *parser) parse() (*media.Asset, error) {
	p.rs = &offsetReader{f: p.file}
	if st, err := p.file.Stat(); err == nil && st.Size() < 8 {
		return nil, p.err(fault.CodeCorruptContainer, 0,
			"file too short (%d bytes)", st.Size())
	}

	// First pass: box tree structure, capturing moov and mdat positions.
	var moov *gomp4.BoxInfo
	var mdatOffset, mdatSize int64
	var mdatCount int

	_, err := gomp4.ReadBoxStructure(p.rs, func(h *gomp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case gomp4.BoxTypeMoov():
			bi := h.BoxInfo
			moov = &bi
		case gomp4.BoxTypeMdat():
			if mdatCount == 0 {
				mdatOffset = int64(h.BoxInfo.Offset)
				mdatSize = int64(h.BoxInfo.Size)
			}
			mdatCount++
		}
		return nil, nil
	})
	if err != nil {
		return nil, p.err(fault.CodeCorruptContainer, p.rs.pos,
			"box walk failed: %v", err)
	}
	if moov == nil {
		return nil, p.err(fault.CodeCorruptContainer, 0, "missing moov box")
	}
	if mdatCount == 0 {
		return nil, p.err(fault.CodeCorruptContainer, 0, "missing mdat box")
	}

	asset := &media.Asset{
		ID:         p.src.AssetID,
		SourcePath: p.src.Path,
		Adapter:    "mp4",
		Version:    Version,
	}
	p.asset = asset

	fileSize := fileSizeOf(p.file)

	// Second pass: walk each trak under moov.
	path := gomp4.BoxPath{gomp4.BoxTypeMoov(), gomp4.BoxTypeTrak()}
	trakInfos, err := gomp4.ExtractBox(p.rs, nil, path)
	if err != nil {
		return nil, p.err(fault.CodeCorruptContainer, p.rs.pos,
			"enumerate trak failed: %v", err)
	}
	for idx, bi := range trakInfos {
		trk, err := p.parseTrak(bi, idx, mdatOffset, mdatSize, fileSize)
		if err != nil {
			return nil, err
		}
		if trk == nil {
			continue
		}
		asset.Tracks = append(asset.Tracks, *trk)
	}
	if len(asset.Tracks) == 0 {
		return nil, p.err(fault.CodeNoPackets, 0, "no usable tracks in moov")
	}
	if err := p.buildPackets(); err != nil {
		return nil, err
	}
	asset.SortPackets()
	for _, t := range asset.Tracks {
		if t.DurationNS > asset.DurationNS {
			asset.DurationNS = t.DurationNS
		}
	}
	if err := clock.ValidateDuration(asset.DurationNS); err != nil {
		return nil, p.err(fault.CodeDurationRange, mdatOffset,
			"asset duration %dns exceeds limit", asset.DurationNS)
	}
	return asset, nil
}

func fileSizeOf(f *os.File) int64 {
	if st, err := f.Stat(); err == nil {
		return st.Size()
	}
	return 0
}

// err builds a located fault at the given offset.
func (p *parser) err(code fault.Code, off int64, format string, args ...any) *fault.Error {
	return fault.New(code, p.loc(off, 0), format, args...).WithOp("mp4.Parse")
}

func (p *parser) loc(off int64, track uint32) fault.Location {
	loc := fault.Location{SourcePath: p.src.Path, AssetID: p.src.AssetID, Adapter: "mp4"}
	if off > 0 {
		loc = loc.WithOffset(off)
	}
	if track != 0 {
		loc = loc.WithTrack(track)
	}
	return loc
}

func (p *parser) errTrack(code fault.Code, off int64, track uint32, format string, args ...any) *fault.Error {
	return fault.New(code, p.loc(off, track), format, args...).WithOp("mp4.Parse")
}

func (p *parser) warn(format string, args ...any) {
	p.asset.Warnings = append(p.asset.Warnings, fmt.Sprintf(format, args...))
}
