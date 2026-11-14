package remux

import (
	"bytes"

	"mp4replay/internal/boxscan"
	"mp4replay/internal/mp4atom"
	"mp4replay/internal/report"
)

// buildMoov constructs a fresh moov for the exported spans. Chunk offsets are
// computed relative to the mdat payload start, which is placed after this moov.
func buildMoov(m *mp4atom.Movie, spans []Span,
	offsets map[int][]int64) ([]byte, error) {
	var moov bytes.Buffer
	mvhd := boxscan.Path(m.Moov, "mvhd")
	if mvhd == nil {
		return nil, report.NewError(report.ExitBoxInvalid, "export", "mvhd missing")
	}
	mvhdBytes, err := boxscan.ReadPayload(m.ReaderAt(), mvhd)
	if err != nil {
		return nil, err
	}
	wBox(&moov, "mvhd", mvhdBytes)
	for ti, sp := range spans {
		trak, err := buildTrak(m, sp, ti, offsets)
		if err != nil {
			return nil, err
		}
		moov.Write(trak)
	}
	return box("moov", moov.Bytes()), nil
}

func buildTrak(m *mp4atom.Movie, sp Span, trackIdx int,
	offsets map[int][]int64) ([]byte, error) {
	tr := sp.Track
	var trak bytes.Buffer
	// tkhd copied verbatim (duration is informational)
	if tkhd := boxscan.Path(tr.Box, "tkhd"); tkhd != nil {
		p, err := boxscan.ReadPayload(m.ReaderAt(), tkhd)
		if err != nil {
			return nil, err
		}
		wBox(&trak, "tkhd", p)
	}
	mdia := boxscan.Child(tr.Box, "mdia")
	var mdiaBuf bytes.Buffer
	for _, name := range []string{"mdhd", "hdlr"} {
		if b := boxscan.Child(mdia, name); b != nil {
			p, err := boxscan.ReadPayload(m.ReaderAt(), b)
			if err != nil {
				return nil, err
			}
			wBox(&mdiaBuf, name, p)
		}
	}
	minf := boxscan.Child(mdia, "minf")
	var minfBuf bytes.Buffer
	if mh := boxscan.Child(minf, "vmhd"); mh != nil {
		copyBox(m, &minfBuf, mh)
	}
	if mh := boxscan.Child(minf, "smhd"); mh != nil {
		copyBox(m, &minfBuf, mh)
	}
	if dinf := boxscan.Child(minf, "dinf"); dinf != nil {
		copyBox(m, &minfBuf, dinf)
	}
	stbl, err := buildStbl(m, sp, trackIdx, offsets)
	if err != nil {
		return nil, err
	}
	minfBuf.Write(stbl)
	wContainer(&mdiaBuf, "minf", minfBuf.Bytes())
	wContainer(&trak, "mdia", mdiaBuf.Bytes())
	return box("trak", trak.Bytes()), nil
}

func copyBox(m *mp4atom.Movie, buf *bytes.Buffer, b *boxscan.Box) {
	p, err := boxscan.ReadPayload(m.ReaderAt(), b)
	if err != nil {
		return
	}
	var kids [][]byte
	for _, c := range b.Children {
		cb, err := serializeTree(m, c)
		if err != nil {
			return
		}
		kids = append(kids, cb)
	}
	if len(kids) > 0 {
		wContainer(buf, b.Type, kids...)
		return
	}
	wBox(buf, b.Type, p)
}

func serializeTree(m *mp4atom.Movie, b *boxscan.Box) ([]byte, error) {
	if len(b.Children) == 0 {
		p, err := boxscan.ReadPayload(m.ReaderAt(), b)
		if err != nil {
			return nil, err
		}
		return box(b.Type, p), nil
	}
	var body bytes.Buffer
	for _, c := range b.Children {
		cb, err := serializeTree(m, c)
		if err != nil {
			return nil, err
		}
		body.Write(cb)
	}
	return box(b.Type, body.Bytes()), nil
}

// buildStbl rewrites every sample table for the selected sample interval.
func buildStbl(m *mp4atom.Movie, sp Span, trackIdx int,
	offsets map[int][]int64) ([]byte, error) {
	tr := sp.Track
	var stbl bytes.Buffer
	if b := boxscan.Child(tr.Stbl, "stsd"); b != nil {
		p, err := boxscan.ReadPayload(m.ReaderAt(), b)
		if err != nil {
			return nil, err
		}
		wBox(&stbl, "stsd", p)
	}
	wBox(&stbl, "stts", buildStts(tr, sp))
	if tr.Table.HasCtts {
		wBox(&stbl, "ctts", buildCtts(tr, sp))
	}
	wBox(&stbl, "stsc", buildStsc())
	wBox(&stbl, "stsz", buildStsz(tr, sp))
	if hasSync(tr, sp) {
		wBox(&stbl, "stss", buildStss(tr, sp))
	}
	wBox(&stbl, "co64", buildCo64(sp, offsets[trackIdx]))
	return box("stbl", stbl.Bytes()), nil
}

func hasSync(tr *mp4atom.Track, sp Span) bool {
	for i := sp.Start; i < sp.End; i++ {
		if tr.Samples[i].Sync {
			return true
		}
	}
	return false
}

func buildStts(tr *mp4atom.Track, sp Span) []byte {
	var b bytes.Buffer
	fullBox(&b, 0, 0)
	// one run per sample keeps the rewrite simple and correct
	wU32(&b, uint32(sp.End-sp.Start))
	for i := sp.Start; i < sp.End; i++ {
		wU32(&b, 1)
		wU32(&b, deltaAt(tr, i))
	}
	return b.Bytes()
}

func deltaAt(tr *mp4atom.Track, i int) uint32 {
	if i+1 < len(tr.Samples) {
		return uint32(tr.Samples[i+1].Dts - tr.Samples[i].Dts)
	}
	if len(tr.Table.Stts) > 0 {
		return tr.Table.Stts[len(tr.Table.Stts)-1].Delta
	}
	return 0
}

func buildCtts(tr *mp4atom.Track, sp Span) []byte {
	var b bytes.Buffer
	fullBox(&b, tr.Table.CttsVer, 0)
	wU32(&b, uint32(sp.End-sp.Start))
	for i := sp.Start; i < sp.End; i++ {
		wU32(&b, 1)
		wU32(&b, uint32(tr.Samples[i].CttsOff))
	}
	return b.Bytes()
}

func buildStsc() []byte {
	var b bytes.Buffer
	fullBox(&b, 0, 0)
	wU32(&b, 1)
	wU32(&b, 1) // first chunk
	wU32(&b, 1) // samples per chunk
	wU32(&b, 1) // sample description index
	return b.Bytes()
}

func buildStsz(tr *mp4atom.Track, sp Span) []byte {
	var b bytes.Buffer
	fullBox(&b, 0, 0)
	wU32(&b, 0) // variable sizes
	wU32(&b, uint32(sp.End-sp.Start))
	for i := sp.Start; i < sp.End; i++ {
		wU32(&b, tr.Samples[i].Size)
	}
	return b.Bytes()
}

func buildStss(tr *mp4atom.Track, sp Span) []byte {
	var b bytes.Buffer
	fullBox(&b, 0, 0)
	var nums []uint32
	for i := sp.Start; i < sp.End; i++ {
		if tr.Samples[i].Sync {
			nums = append(nums, uint32(i-sp.Start+1))
		}
	}
	wU32(&b, uint32(len(nums)))
	for _, n := range nums {
		wU32(&b, n)
	}
	return b.Bytes()
}

// buildCo64 emits placeholder zeros; offsets are patched after mdat layout is
// known via patchOffsets. One chunk per sample is used.
func buildCo64(sp Span, offs []int64) []byte {
	var b bytes.Buffer
	fullBox(&b, 0, 0)
	count := sp.End - sp.Start
	wU32(&b, uint32(count))
	for i := 0; i < count; i++ {
		var o int64
		if i < len(offs) {
			o = offs[i]
		}
		wU64(&b, uint64(o))
	}
	return b.Bytes()
}
