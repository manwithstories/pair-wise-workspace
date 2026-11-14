package fixture

import (
	"bytes"
)

// DefaultMovieTS is the mvhd timescale used by synthesized files.
const DefaultMovieTS uint32 = 90000

// Generate writes a complete MP4 fixture according to opts.
func Generate(opts Options) error {
	if opts.Variant == "" {
		opts.Variant = VariantElstCttsWrap
	}
	specs := []*trackSpec{buildVideoSpec(opts.Frames, opts.Variant)}
	if opts.Tracks >= 2 {
		specs = append(specs, buildAudioSpec(opts.Frames))
	}
	if opts.SamplesPerChunk > 1 {
		for _, sp := range specs {
			sp.perChunk = opts.SamplesPerChunk
		}
	}

	// First pass: build moov with placeholder offsets to learn its size, then
	// compute real mdat-relative offsets and rebuild.
	moovSize, err := measureMoov(specs, opts)
	if err != nil {
		return err
	}
	ftyp := ftypBox()
	mdatHeader := int64(8)
	var base int64
	if opts.MoovAtEnd {
		base = int64(len(ftyp)) + mdatHeader
	} else {
		base = int64(len(ftyp)) + moovSize + mdatHeader
	}
	if err := assignOffsets(specs, base); err != nil {
		return err
	}
	moov, err := buildMoov(specs, opts)
	if err != nil {
		return err
	}
	mdat := buildMdat(specs)
	return writeFile(opts.Path, ftyp, moov, mdat, opts.MoovAtEnd)
}

func ftypBox() []byte {
	var b builder
	b.fourcc("isom")
	b.u32(0x200)
	b.fourcc("isom")
	b.fourcc("iso2")
	b.fourcc("avc1")
	b.fourcc("mp41")
	return box("ftyp", b.Bytes())
}

func measureMoov(specs []*trackSpec, opts Options) (int64, error) {
	for _, sp := range specs {
		sp.offsets = make([]int64, sp.chunkCount())
	}
	m, err := buildMoov(specs, opts)
	if err != nil {
		return 0, err
	}
	return int64(len(m)), nil
}

func assignOffsets(specs []*trackSpec, base int64) error {
	cursor := base
	for _, sp := range specs {
		sp.offsets = make([]int64, 0, sp.chunkCount())
		for i, s := range sp.samples {
			if i%sp.perChunk == 0 {
				sp.offsets = append(sp.offsets, cursor)
			}
			cursor += int64(len(s))
		}
	}
	return nil
}

// chunkCount is the number of chunks given the per-chunk packing.
func (sp *trackSpec) chunkCount() int {
	n := len(sp.samples)
	return (n + sp.perChunk - 1) / sp.perChunk
}

func buildMdat(specs []*trackSpec) []byte {
	var body bytes.Buffer
	for _, sp := range specs {
		for _, s := range sp.samples {
			body.Write(s)
		}
	}
	return box("mdat", body.Bytes())
}

func buildMoov(specs []*trackSpec, opts Options) ([]byte, error) {
	movieTS := DefaultMovieTS
	traks := make([][]byte, 0, len(specs))
	var maxMovieDur uint64
	withElst := opts.Variant == VariantElstCttsWrap
	for _, sp := range specs {
		mediaDur := mediaDuration(sp)
		trak, err := buildTrak(sp, opts.UseCo64, opts.CttsV0,
			withElst, movieTS, mediaDur)
		if err != nil {
			return nil, err
		}
		traks = append(traks, trak)
		if d := scaleToMovie(mediaDur, sp.ts, movieTS); d > maxMovieDur {
			maxMovieDur = d
		}
	}
	mvhd, err := mvhdBox(movieTS, maxMovieDur)
	if err != nil {
		return nil, err
	}
	kids := append([][]byte{mvhd}, traks...)
	return container("moov", kids...), nil
}

func mediaDuration(sp *trackSpec) uint64 {
	var sum uint64
	for _, d := range sp.deltas {
		sum += uint64(d)
	}
	return sum
}
