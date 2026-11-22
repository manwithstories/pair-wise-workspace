package testdata

import (
	"mediarun/internal/ingest"
)

// buildMXF writes the OP1a-subset framing that ingest.OpenMXF understands:
// one partition pack with tagged descriptors, then one KLV per essence
// sample (video and audio interleaved).
func buildMXF(spec FixtureSpec) ([]byte, error) {
	var out []byte
	var pack []byte

	// Flatten every track group's tag items into one ordered descriptor list.
	var items [][]byte
	items = append(items, mxfDescriptors("video", "vcb1", spec.VideoScale,
		spec.Width, spec.Height, 0, spec.VideoSamples)...)
	items = append(items, mxfDescriptors("audio", "pcm1", spec.AudioRate,
		0, 0, 2, spec.AudioSamples)...)
	pack = append(pack, ber(int64(len(items)))...)
	for _, item := range items {
		pack = append(pack, item...)
	}
	out = append(out, ingest.MXFPartitionULForTest()...)
	out = append(out, ber(int64(len(pack)))...)
	out = append(out, pack...)

	var videoPayloads, audioPayloads [][]byte
	if spec.TargetBitrate > 0 {
		// MXF subset uses one edit unit per sample at VideoScale ticks/sec.
		totalBytes := spec.TargetBitrate * int64(spec.VideoSamples) / spec.VideoScale / 8
		videoBytes := totalBytes * 90 / 100
		videoPayloads = SizedPayloads(0, spec.VideoSamples, videoBytes)
		audioPayloads = SizedPayloads(1, spec.AudioSamples, totalBytes-videoBytes)
	}

	// Interleave samples deterministically: video first, then audio.
	for i := 0; i < spec.VideoSamples; i++ {
		var p []byte
		if videoPayloads != nil {
			p = videoPayloads[i]
		} else {
			p = videoPayload(int64(i), spec.Width, spec.Height)
		}
		out = append(out, ingest.MXFVideoULForTest()...)
		out = append(out, ber(int64(len(p)))...)
		out = append(out, p...)
	}
	for i := 0; i < spec.AudioSamples; i++ {
		var p []byte
		if audioPayloads != nil {
			p = audioPayloads[i]
		} else {
			p = audioPayload(int64(i))
		}
		out = append(out, ingest.MXFAudioULForTest()...)
		out = append(out, ber(int64(len(p)))...)
		out = append(out, p...)
	}
	return out, nil
}

// mxfDescriptors builds the ordered tag items for one track group.
func mxfDescriptors(kind, codec string, scale int64, w, h uint32, ch uint16, n int) [][]byte {
	items := [][]byte{
		mxfTag(2, []byte(kind)),
		mxfTag(3, []byte(codec)),
		mxfTag(4, ber(scale)),
	}
	if kind == "video" {
		items = append(items, mxfTag(5, ber(int64(w))), mxfTag(6, ber(int64(h))))
	} else {
		items = append(items, mxfTag(7, ber(int64(ch))))
	}
	items = append(items, mxfTag(8, ber(int64(n))))
	return items
}

func mxfTag(tag byte, body []byte) []byte {
	out := []byte{tag}
	out = append(out, ber(int64(len(body)))...)
	return append(out, body...)
}

// ber encodes an SMPTE BER length/value using short form when possible.
func ber(v int64) []byte {
	if v < 0x80 {
		return []byte{byte(v)}
	}
	var raw []byte
	for x := v; x > 0; x >>= 8 {
		raw = append([]byte{byte(x)}, raw...)
	}
	return append([]byte{0x80 | byte(len(raw))}, raw...)
}
