package scan

import (
	"context"
	"fmt"
	"os"

	mp4 "github.com/abema/go-mp4"

	merr "mediaroute/internal/errors"
	"mediaroute/internal/manifest"
)

// trackMeta is the raw MP4 metadata needed to build a timeline index.
type trackMeta struct {
	trackID   uint32
	timescale uint32
	kind      string
	movieTS   uint32
	elst      []mp4.ElstEntry
	stts      *mp4.Stts
	ctts      *mp4.Ctts
	stsc      *mp4.Stsc
	stsz      *mp4.Stsz
	chunks    []uint64
	stss      []uint32
	entry     mp4Entry
}

type mp4Entry struct {
	typ        string
	width      int
	height     int
	sampleRate int
	channels   int
}

// readFileMeta parses the moov box of one MP4 source.
func readFileMeta(path string) (*trackMeta, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, merr.New(merr.KindScan, 0, -1, -1, false, "open %s: %v", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, merr.New(merr.KindScan, 0, -1, -1, false, "stat %s: %v", path, err)
	}
	root, err := mp4.ExtractBoxesWithPayload(f, nil, []mp4.BoxPath{
		{mp4.BoxTypeMoov(), mp4.BoxTypeMvhd()},
		{mp4.BoxTypeMoov(), mp4.BoxTypeTrak()},
	})
	if err != nil {
		return nil, 0, merr.New(merr.KindScan, 0, -1, -1, false, "parse %s: %v", path, err)
	}
	meta := &trackMeta{}
	var trakInfos []*mp4.BoxInfo
	for _, bip := range root {
		switch bip.Info.Type {
		case mp4.BoxTypeMvhd():
			mvhd, ok := bip.Payload.(*mp4.Mvhd)
			if !ok {
				return nil, 0, merr.New(merr.KindScan, 0, -1, int64(bip.Info.Offset), false, "unexpected mvhd payload")
			}
			meta.movieTS = mvhd.Timescale
		case mp4.BoxTypeTrak():
			trakInfos = append(trakInfos, &bip.Info)
		}
	}
	if len(trakInfos) == 0 {
		return nil, 0, merr.New(merr.KindScan, 0, -1, 0, false, "%s contains no tracks", path)
	}
	// Use the first track; fixtures and demos write one track per file. If a
	// file has several, the first is the one the manifest names.
	if err := readTrak(f, trakInfos[0], meta); err != nil {
		return nil, 0, err
	}
	return meta, st.Size(), nil
}

func readTrak(f *os.File, trak *mp4.BoxInfo, meta *trackMeta) error {
	boxes, err := mp4.ExtractBoxesWithPayload(f, trak, []mp4.BoxPath{
		{mp4.BoxTypeTkhd()},
		{mp4.BoxTypeEdts(), mp4.BoxTypeElst()},
		{mp4.BoxTypeMdia(), mp4.BoxTypeMdhd()},
		{mp4.BoxTypeMdia(), mp4.BoxTypeHdlr()},
		{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), mp4.BoxTypeAvc1()},
		{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), mp4.BoxTypeMp4a()},
		{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStts()},
		{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeCtts()},
		{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsc()},
		{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsz()},
		{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStco()},
		{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeCo64()},
		{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStss()},
	})
	if err != nil {
		return merr.New(merr.KindScan, 0, -1, int64(trak.Offset), false, "read trak: %v", err)
	}
	var handler [4]byte
	var stco *mp4.Stco
	var co64 *mp4.Co64
	for _, bip := range boxes {
		off := int64(bip.Info.Offset)
		switch bip.Info.Type {
		case mp4.BoxTypeTkhd():
			meta.trackID = bip.Payload.(*mp4.Tkhd).TrackID
		case mp4.BoxTypeElst():
			e := bip.Payload.(*mp4.Elst)
			for i := range e.Entries {
				meta.elst = append(meta.elst, e.Entries[i])
			}
		case mp4.BoxTypeMdhd():
			meta.timescale = bip.Payload.(*mp4.Mdhd).Timescale
		case mp4.BoxTypeHdlr():
			handler = bip.Payload.(*mp4.Hdlr).HandlerType
		case mp4.BoxTypeAvc1():
			ase := bip.Payload.(*mp4.VisualSampleEntry)
			meta.entry = mp4Entry{typ: "avc1", width: int(ase.Width), height: int(ase.Height)}
		case mp4.BoxTypeMp4a():
			ase := bip.Payload.(*mp4.AudioSampleEntry)
			meta.entry = mp4Entry{typ: "mp4a", sampleRate: int(ase.SampleRate >> 16), channels: int(ase.ChannelCount)}
		case mp4.BoxTypeStts():
			meta.stts = bip.Payload.(*mp4.Stts)
		case mp4.BoxTypeCtts():
			meta.ctts = bip.Payload.(*mp4.Ctts)
		case mp4.BoxTypeStsc():
			meta.stsc = bip.Payload.(*mp4.Stsc)
		case mp4.BoxTypeStsz():
			meta.stsz = bip.Payload.(*mp4.Stsz)
		case mp4.BoxTypeStco():
			stco = bip.Payload.(*mp4.Stco)
		case mp4.BoxTypeCo64():
			co64 = bip.Payload.(*mp4.Co64)
		case mp4.BoxTypeStss():
			meta.stss = append(meta.stss, bip.Payload.(*mp4.Stss).SampleNumber...)
		default:
			_ = off
		}
	}
	if stco != nil {
		for _, o := range stco.ChunkOffset {
			meta.chunks = append(meta.chunks, uint64(o))
		}
	} else if co64 != nil {
		meta.chunks = append(meta.chunks, co64.ChunkOffset...)
	} else {
		return merr.New(merr.KindScan, meta.trackID, -1, int64(trak.Offset), false, "no stco/co64 chunk offsets")
	}
	switch handler {
	case [4]byte{'v', 'i', 'd', 'e'}:
		meta.kind = manifest.KindVideo
	case [4]byte{'s', 'o', 'u', 'n'}:
		meta.kind = manifest.KindAudio
	default:
		return merr.New(merr.KindScan, meta.trackID, -1, int64(trak.Offset), false, "unsupported handler %q", string(handler[:]))
	}
	if meta.stts == nil {
		return merr.New(merr.KindScan, meta.trackID, -1, int64(trak.Offset), false, "missing stts")
	}
	if meta.stsc == nil {
		return merr.New(merr.KindScan, meta.trackID, -1, int64(trak.Offset), false, "missing stsc")
	}
	if meta.stsz == nil {
		return merr.New(merr.KindScan, meta.trackID, -1, int64(trak.Offset), false, "missing stsz")
	}
	if meta.timescale == 0 {
		return merr.New(merr.KindScan, meta.trackID, -1, int64(trak.Offset), false, "zero media timescale")
	}
	if len(meta.stsz.EntrySize) > MaxSamplesPerTrack {
		return merr.New(merr.KindScan, meta.trackID, -1, int64(trak.Offset), false,
			"sample count %d exceeds limit %d", len(meta.stsz.EntrySize), MaxSamplesPerTrack)
	}
	return nil
}

// readSampleBytes validates a sample's byte range, optionally reading it.
func readSampleBytes(ctx context.Context, f *os.File, off int64, size uint32, fileSize int64, wantData bool) (bool, []byte, error) {
	if off < 0 || int64(size) < 0 {
		return false, nil, merr.New(merr.KindScan, 0, -1, off, false, "negative sample range")
	}
	if off > fileSize || int64(size) > fileSize-off {
		return false, nil, nil
	}
	if !wantData {
		return true, nil, nil
	}
	if size > MaxReadSampleBytes {
		return false, nil, merr.New(merr.KindScan, 0, -1, off, false, "sample size %d exceeds read limit %d", size, MaxReadSampleBytes)
	}
	if err := ctx.Err(); err != nil {
		return false, nil, canceled(err)
	}
	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, off); err != nil {
		return false, nil, merr.New(merr.KindScan, 0, -1, off, true, "read sample: %v", err)
	}
	return true, buf, nil
}

// allZero reports whether a payload carries no information (a zeroed page).
func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

var _ = fmt.Sprintf
