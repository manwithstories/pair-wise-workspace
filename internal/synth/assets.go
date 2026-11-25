// Package synth generates bounded, deterministic local oral-history fixtures.
package synth

import (
	"math/rand"

	"archivectl/internal/container"
)

const (
	videoTimescale = 90_000
	frameDuration  = 3000 // 30 fps
	frameBytes     = 5000 // 5000*8 / (3000/90000) = 1200 kbps
)

func deterministicBytes(r *rand.Rand, n int, marker byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Intn(256))
	}
	if n > 0 {
		b[0] = marker
	}
	return b
}

func seiPayload(r *rand.Rand) []byte {
	// Annex-B start code + H.264 NAL type 6, followed by a filler NAL so the
	// SEI extractor sees a bounded NAL interval.
	b := []byte{0, 0, 0, 1, 0x06, 0x05, 0x10}
	b = append(b, deterministicBytes(r, 16, 0x41)...)
	b = append(b, 0, 0, 0, 1, 0x0c)
	b = append(b, deterministicBytes(r, 24, 0x5a)...)
	if len(b) > frameBytes {
		return b[:frameBytes]
	}
	return append(b, deterministicBytes(r, frameBytes-len(b), 0x33)...)
}

func normalAsset(seed int64) *container.Asset {
	r := rand.New(rand.NewSource(seed))
	const frames = 120
	video := make([]container.Sample, frames)
	for i := range video {
		var data []byte
		if i == 0 {
			data = seiPayload(r)
		} else {
			data = deterministicBytes(r, frameBytes, byte(0x20+i))
			// Add an in-band SEI to a middle keyframe as well.
			if i == 6 {
				copy(data, seiPayload(r))
			}
		}
		video[i] = container.Sample{
			Index: i, Data: data, Size: len(data),
			DTS: int64(i * frameDuration), PTS: int64(i*frameDuration + 900),
			Duration: frameDuration, Flags: 0x010000,
		}
	}

	subs := []container.Sample{
		textSample(0, 0, "修复口述史影像：第一盘磁带"),
		textSample(1, 1000, "采访者：请描述当年的阅览室"),
		textSample(2, 3000, "受访者：这些章节标记仍然对齐"),
	}
	chapters := []container.Sample{
		textSample(0, 0, "第一章 磁带 A 面"),
		textSample(1, 2000, "第二章 迁移记录"),
	}
	id3a := append([]byte("ID3\x04\x00\x00"), deterministicBytes(r, 40, 0x49)...)
	id3b := append([]byte("ID3\x03\x00\x00"), deterministicBytes(r, 40, 0x49)...)
	meta := []container.Sample{
		{Index: 0, Data: id3a, Size: len(id3a), DTS: 0, PTS: 500, Duration: 1000},
		{Index: 1, Data: id3b, Size: len(id3b), DTS: 2400, PTS: 2500, Duration: 1000},
	}
	emsgBox := container.BuildEMSG("urn:archivectl:oral-history", "tape-reel", 1000, 100, 2000, 41,
		[]byte("reel-change-approaching"))

	return &container.Asset{
		Tracks: []container.Track{
			{ID: 1, Kind: container.KindVideo, Codec: "avc1", Handler: "vide", Timescale: videoTimescale, Samples: video, References: []uint32{2}},
			{ID: 2, Kind: container.KindSubtitle, Codec: "tx3g", Handler: "sbtl", Timescale: 1000, Samples: subs},
			{ID: 3, Kind: container.KindChapters, Codec: "chpl", Handler: "chap", Timescale: 1000, Samples: chapters},
			{ID: 4, Kind: container.KindMetadata, Codec: "ID32", Handler: "meta", Timescale: 1000, Samples: meta},
		},
		Events: []container.Event{{
			Kind: container.EventEMSG, TimeNS: 100_000_000, Timescale: 1000, RawTime: 100,
			Label: "urn:archivectl:oral-history/tape-reel/id=41", Payload: []byte("reel-change-approaching"),
			RawBox: emsgBox, Hash: container.HashBytes([]byte("reel-change-approaching")),
		}},
	}
}

func textSample(index int, pts int64, text string) container.Sample {
	b := []byte(text)
	return container.Sample{
		Index: index, Data: b, Size: len(b), DTS: pts, PTS: pts, Duration: 1000,
	}
}

func fragmentTrack(seed int64, fragIndex int) container.Track {
	r := rand.New(rand.NewSource(seed + int64(fragIndex)*1009))
	base := int64(fragIndex * 6000)
	samples := make([]container.Sample, 2)
	for i := range samples {
		data := deterministicBytes(r, 420, byte(fragIndex*16+i+1))
		dts := base + int64(i*frameDuration)
		samples[i] = container.Sample{
			Index: i, Data: data, Size: len(data), DTS: dts, PTS: dts + 450,
			Duration: frameDuration, Flags: 0x010000,
		}
	}
	return container.Track{ID: 1, Kind: container.KindVideo, Codec: "avc1", Timescale: videoTimescale, Samples: samples}
}
