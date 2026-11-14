package fixture

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"mp4tool/internal/box"
)

// Targets for normal.mp4 required by the acceptance spec.
const (
	normalMoovOffset = 48213
	normalBoxCount   = 312
)

func videoStbl(sizes []uint32, stts [][2]uint32, extra []byte, keys []uint32, perChunk uint32, stco []uint64) []byte {
	parts := [][]byte{stsdVideoBox(), sttsBox(stts)}
	if extra != nil {
		parts = append(parts, extra)
	}
	parts = append(parts, stssBox(keys), stszBox(sizes), stscBox(perChunk), stcoBox(stco))
	return bx("stbl", concat(parts...))
}

func videoTrak(id, tkhdDur, mdhdDur uint32, edts, stbl []byte) []byte {
	mdia := bx("mdia", concat(mdhdBox(mdhdDur), hdlrBox("vide", "VideoHandler"), minf(vmhdBox(), stbl)))
	if edts != nil {
		return bx("trak", concat(tkhdBox(id, tkhdDur, 320, 240, 0), edts, mdia))
	}
	return bx("trak", concat(tkhdBox(id, tkhdDur, 320, 240, 0), mdia))
}

func audioTrak(id, tkhdDur, mdhdDur uint32, stco []uint64) []byte {
	sizes := make([]uint32, 300)
	for i := range sizes {
		sizes[i] = 40
	}
	stbl := bx("stbl", concat(
		stsdAudioBox(), sttsBox([][2]uint32{{300, frameDelta}}),
		stszBox(sizes), stscBox(300), stcoBox(stco),
	))
	mdia := bx("mdia", concat(mdhdBox(mdhdDur), hdlrBox("soun", "SoundHandler"), minf(smhdBox(), stbl)))
	return bx("trak", concat(tkhdBox(id, tkhdDur, 0, 0, 0x0100), mdia))
}

func mdatBox(payload []byte) []byte { return bx("mdat", payload) }

func videoPayload(sizes []uint32) []byte {
	var out []byte
	for i, s := range sizes {
		out = append(out, sampleBytes(i, int(s))...)
	}
	return out
}

func audioPayload() []byte {
	var out []byte
	for i := 0; i < 300; i++ {
		out = append(out, sampleBytes(10000+i, 40)...)
	}
	return out
}

// normalMP4 builds the tail-moov reference file: 2 traks, 300 video
// samples (ctts delay of 3 frames via zero-delta stts head), 25
// keyframes, one elst segment, exact moov offset and box count.
func normalMP4() ([]byte, error) {
	sizes := keySizes(300, 12, 200, 100)
	mdatSize := 8 + sumSizes(sizes) + 12000
	freesTotal := int64(normalMoovOffset) - 24 - int64(mdatSize)
	if freesTotal < 8 {
		return nil, fmt.Errorf("fixture layout underflow")
	}
	dataStart := uint64(24 + freesTotal + 8)
	chunk0 := dataStart
	chunk1 := chunk0 + sumSizes(sizes[0:100])
	chunk2 := chunk1 + sumSizes(sizes[100:200])
	audioOff := chunk2 + sumSizes(sizes[200:300])
	vStbl := videoStbl(sizes, [][2]uint32{{3, 0}, {297, frameDelta}},
		ctts0Box(300, 21600), keyList(300, 12), 100, []uint64{chunk0, chunk1, chunk2})
	edts := bx("edts", elstBox(1, []elstEnt{{Dur: 2160000, Media: 0, Rate: 0x10000}}))
	vTrak := videoTrak(1, 2160000, 2138400, edts, vStbl)
	aTrak := audioTrak(2, 2160000, 2160000, []uint64{audioOff})
	moov := bx("moov", concat(mvhdBox(2160000, 3), vTrak, aTrak))
	moovBoxes, err := box.CountBoxes(bytes.NewReader(moov), 0, uint64(len(moov)), 0)
	if err != nil {
		return nil, err
	}
	nSmall := normalBoxCount - 3 - moovBoxes // ftyp, adjustable free, mdat
	adj := int(freesTotal) - 8*nSmall
	if nSmall < 0 || adj < 8 {
		return nil, fmt.Errorf("cannot reach box target: nSmall=%d adj=%d", nSmall, adj)
	}
	var file []byte
	file = append(file, ftypBox()...)
	for i := 0; i < nSmall; i++ {
		file = append(file, freeBox(8)...)
	}
	file = append(file, freeBox(adj)...)
	file = append(file, mdatBox(concat(videoPayload(sizes), audioPayload()))...)
	file = append(file, moov...)
	return file, verify(file, normalMoovOffset, normalBoxCount)
}

// frontMP4 builds a moov-before-mdat file (30 samples, single trak).
func frontMP4() ([]byte, error) {
	sizes := keySizes(30, 12, 150, 150)
	stbl := func(off uint64) []byte {
		return videoStbl(sizes, [][2]uint32{{30, frameDelta}}, nil, keyList(30, 12), 30, []uint64{off})
	}
	moov := bx("moov", concat(mvhdBox(216000, 2), videoTrak(1, 216000, 216000, nil, stbl(0))))
	stco := uint64(24+len(moov)) + 8
	moov = bx("moov", concat(mvhdBox(216000, 2), videoTrak(1, 216000, 216000, nil, stbl(stco))))
	file := concat(ftypBox(), moov, mdatBox(videoPayload(sizes)))
	if _, _, err := box.FindMoov(bytes.NewReader(file), uint64(len(file))); err != nil {
		return nil, err
	}
	return file, nil
}

// elstMP4 builds a file with an empty edit, a media-offset segment and
// a 2x-rate segment (ISO BMFF elst version 1).
func elstMP4() ([]byte, error) {
	sizes := keySizes(100, 10, 150, 150)
	stbl := videoStbl(sizes, [][2]uint32{{100, frameDelta}}, nil, keyList(100, 10), 100, []uint64{32})
	edts := bx("edts", elstBox(1, []elstEnt{
		{Dur: 36000, Media: -1, Rate: 0x10000},
		{Dur: 324000, Media: 72000, Rate: 0x10000},
		{Dur: 90000, Media: 396000, Rate: 0x20000},
	}))
	moov := bx("moov", concat(mvhdBox(450000, 2), videoTrak(1, 450000, 720000, edts, stbl)))
	file := concat(ftypBox(), mdatBox(videoPayload(sizes)), moov)
	if _, _, err := box.FindMoov(bytes.NewReader(file), uint64(len(file))); err != nil {
		return nil, err
	}
	return file, nil
}

// wrapMP4 builds a file whose QuickTime elst v0 media_time is a
// wrapped 32-bit unsigned value (restores to -1800 ticks, one wrap
// event; negative starts clamp to 0).
func wrapMP4() ([]byte, error) {
	sizes := keySizes(100, 12, 3520, 3200)
	stbl := videoStbl(sizes, [][2]uint32{{100, frameDelta}}, nil, keyList(100, 12), 100, []uint64{32})
	edts := bx("edts", elstBox(0, []elstEnt{{Dur: 720000, Media: -1800, Rate: 0x10000}}))
	moov := bx("moov", concat(mvhdBox(720000, 2), videoTrak(1, 720000, 720000, edts, stbl)))
	file := concat(ftypBox(), mdatBox(videoPayload(sizes)), moov)
	if _, _, err := box.FindMoov(bytes.NewReader(file), uint64(len(file))); err != nil {
		return nil, err
	}
	return file, nil
}

// corruptMP4 returns a file whose moov declares a size beyond EOF.
func corruptMP4() []byte {
	head := make([]byte, 8)
	binary.BigEndian.PutUint32(head, 4096)
	copy(head[4:8], "moov")
	return concat(ftypBox(), head, make([]byte, 64))
}

// GenerateAll writes every fixture into dir and returns the file names.
func GenerateAll(dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	normal, err := normalMP4()
	if err != nil {
		return nil, fmt.Errorf("normal: %w", err)
	}
	front, err := frontMP4()
	if err != nil {
		return nil, fmt.Errorf("front: %w", err)
	}
	elst, err := elstMP4()
	if err != nil {
		return nil, fmt.Errorf("elst: %w", err)
	}
	wrap, err := wrapMP4()
	if err != nil {
		return nil, fmt.Errorf("wrap: %w", err)
	}
	files := map[string][]byte{
		"normal.mp4":  normal,
		"front.mp4":   front,
		"elst.mp4":    elst,
		"wrap.mp4":    wrap,
		"corrupt.mp4": corruptMP4(),
	}
	names := []string{"normal.mp4", "front.mp4", "elst.mp4", "wrap.mp4", "corrupt.mp4"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), files[name], 0o644); err != nil {
			return nil, err
		}
	}
	return names, nil
}
