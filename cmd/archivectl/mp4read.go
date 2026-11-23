package main

import (
	"os"

	"github.com/abema/go-mp4"

	"archive/internal/media"
)

// mp4TrackInfo is the per-track sample layout parsed back from the
// container: timescale plus each sample's size and absolute byte offset.
type mp4TrackInfo struct {
	timescale uint32
	sizes     []uint32
	offsets   []uint64
}

// readMP4 extracts mdhd/stsz/stco per trak with go-mp4 and expands the
// single-chunk stco into per-sample offsets. Tracks are returned in file
// order, matching the manifest's track order.
func readMP4(path string) ([]mp4TrackInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, media.New(media.KindInternal, "mp4.read", "", -1, "open", err)
	}
	defer f.Close()
	type gather struct {
		mdhd []*mp4.BoxInfoWithPayload
		stsz []*mp4.BoxInfoWithPayload
		stco []*mp4.BoxInfoWithPayload
	}
	g := gather{}
	paths := map[*[]*mp4.BoxInfoWithPayload]mp4.BoxPath{
		&g.mdhd: {mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMdhd()},
		&g.stsz: {mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(),
			mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsz()},
		&g.stco: {mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(),
			mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStco()},
	}
	for dst, p := range paths {
		boxes, err := mp4.ExtractBoxesWithPayload(f, nil, []mp4.BoxPath{p})
		if err != nil {
			return nil, media.Corruptf("mp4.read", "", -1, "extract boxes: "+err.Error())
		}
		*dst = boxes
	}
	if len(g.mdhd) == 0 || len(g.mdhd) != len(g.stsz) || len(g.mdhd) != len(g.stco) {
		return nil, media.Corruptf("mp4.read", "", -1,
			"inconsistent trak box counts (mdhd/stsz/stco)")
	}
	var out []mp4TrackInfo
	for i := range g.mdhd {
		mdhd := g.mdhd[i].Payload.(*mp4.Mdhd)
		stsz := g.stsz[i].Payload.(*mp4.Stsz)
		stco := g.stco[i].Payload.(*mp4.Stco)
		if len(stco.ChunkOffset) != 1 {
			return nil, media.New(media.KindUnsupported, "mp4.read", "", -1,
				"multi-chunk stco not supported", nil)
		}
		ti := mp4TrackInfo{timescale: mdhd.Timescale}
		off := uint64(stco.ChunkOffset[0])
		for _, sz := range stsz.EntrySize {
			ti.sizes = append(ti.sizes, sz)
			ti.offsets = append(ti.offsets, off)
			off += uint64(sz)
		}
		out = append(out, ti)
	}
	return out, nil
}
