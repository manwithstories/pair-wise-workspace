// Package fixture generates every demo and damage sample in-process so the
// project never depends on external media files. All generators are
// deterministic and emit valid (or deliberately damaged) Matroska-shaped
// EBML byte streams consumed by the real parser.
package fixture

import (
	"bytes"

	"mkvarchive/internal/ebml"
	"mkvarchive/internal/model"
)

// Case selects which sample variant to generate.
type Case string

const (
	CaseStandard           Case = "standard"
	CaseTruncated          Case = "truncated"
	CaseUnknownBlock       Case = "unknown-block"
	CaseChannelMismatch    Case = "channels"
	CaseSampleRateConflict Case = "samplerate"
	CaseColorMissing       Case = "color"
)

// Sample is one generated input together with its expected outcome.
type Sample struct {
	Name          string
	Data          []byte
	BlockCount    int // fully recoverable blocks for the damaged variants
	ExpectWarning string
}

// DemoClusters is the default cluster count for demo/recover runs, keeping
// the quality-block count inside the 100..500 acceptance range.
const DemoClusters = 60

// DurationNS is the fixed timeline length generated for clusterCount clusters.
func DurationNS(clusterCount int) int64 {
	return int64(clusterCount) * clusterStepMS * 1_000_000
}

func u(id uint32, v uint64) []byte {
	return ebml.EncodeElement(id, ebml.EncodeUnsigned(v))
}

func f(id uint32, v float64) []byte {
	return ebml.EncodeElement(id, ebml.EncodeFloat(v))
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// trackEntry builds a minimal TrackEntry; override fields per case.
func trackEntry(uid, number uint64, tt model.TrackType, codec string, extra []byte) []byte {
	entry := cat(
		u(model.IDTrackUID, uid),
		u(model.IDTrackNumber, number),
		u(model.IDTrackType, uint64(tt)),
		ebml.EncodeElement(model.IDCodecID, []byte(codec)),
		extra,
	)
	return ebml.EncodeElement(model.IDTrackEntry, entry)
}

func simpleBlock(trackNum uint64, relTC int16, flags byte, frame []byte) []byte {
	tn, _ := ebml.EncodeVINT(trackNum - 1)
	out := append([]byte{}, tn...)
	out = append(out, byte(uint16(relTC)>>8), byte(uint16(relTC)), flags)
	return append(out, frame...)
}

// Frame sizes are chosen so one cluster carries 150,000 payload bytes and a
// 100 ms cluster step yields a 12,000 kbps stream for the acceptance band.
// At TimestampScale 1,000,000, 100 raw timecode units equal 100 ms.
const (
	videoFrameBytes = 148_976
	audioFrameBytes = 1_024
	clusterStepMS   = 100 // raw timecode units; scale 1,000,000 makes this 100 ms
)

// frameBytes produces a deterministic, compressible-looking frame payload.
func frameBytes(seed, size int) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = byte((seed*31 + i*7) & 0xFF)
	}
	return out
}

// Generate builds the selected sample. blockCount controls how many
// interleaved audio/video clusters are emitted.
func Generate(c Case, blockCount int) Sample {
	switch c {
	case CaseTruncated:
		return generateTruncated(blockCount)
	case CaseUnknownBlock:
		return generateUnknownBlock(blockCount)
	case CaseChannelMismatch:
		return generateChannelMismatch(blockCount)
	case CaseSampleRateConflict:
		return generateSampleRateConflict(blockCount)
	case CaseColorMissing:
		return generateColorMissing(blockCount)
	default:
		return generateStandard(blockCount)
	}
}

func baseTracks(c Case) []byte {
	var videoExtra, audioExtra []byte
	switch c {
	case CaseColorMissing:
		// Video element present but no Colour child at all.
		videoExtra = ebml.EncodeElement(model.IDVideo,
			cat(u(model.IDPixelWidth, 640), u(model.IDPixelHeight, 360)))
	case CaseChannelMismatch:
		audioExtra = ebml.EncodeElement(model.IDAudio,
			cat(u(model.IDChannels, 6), f(model.IDSamplingFreq, 48000),
				ebml.EncodeElement(model.IDChannelPositions,
					make([]byte, 8)))) // only 2 angles for 6 declared channels
	case CaseSampleRateConflict:
		audioExtra = ebml.EncodeElement(model.IDAudio,
			cat(u(model.IDChannels, 2), f(model.IDSamplingFreq, 44100)))
	default:
		videoExtra = ebml.EncodeElement(model.IDVideo,
			cat(u(model.IDPixelWidth, 640), u(model.IDPixelHeight, 360),
				ebml.EncodeElement(model.IDColour,
					cat(u(model.IDColorMatrix, 1), u(model.IDColorRange, 1),
						u(model.IDColorPrimaries, 1), u(model.IDTransferChar, 1)))))
		if c != CaseSampleRateConflict {
			audioExtra = ebml.EncodeElement(model.IDAudio,
				cat(u(model.IDChannels, 2), f(model.IDSamplingFreq, 48000)))
		}
	}
	video := trackEntry(1, 1, model.TrackTypeVideo, "V_VP9", videoExtra)
	if c == CaseSampleRateConflict {
		// Two audio tracks declaring different rates.
		a1 := trackEntry(2, 2, model.TrackTypeAudio, "A_OPUS",
			ebml.EncodeElement(model.IDAudio,
				cat(u(model.IDChannels, 2), f(model.IDSamplingFreq, 48000))))
		a2 := trackEntry(3, 3, model.TrackTypeAudio, "A_OPUS", audioExtra)
		return cat(video, a1, a2)
	}
	audio := trackEntry(2, 2, model.TrackTypeAudio, "A_OPUS", audioExtra)
	return cat(video, audio)
}

func segmentBody(c Case, blockCount int, unknownCluster bool) []byte {
	info := ebml.EncodeElement(model.IDSegmentInfo,
		u(model.IDTimestampScale, 1_000_000))
	tracks := ebml.EncodeElement(model.IDTracks, baseTracks(c))
	var body bytes.Buffer
	body.Write(info)
	body.Write(tracks)
	for i := 0; i < blockCount; i++ {
		tc := uint64(i * clusterStepMS)
		var cluster bytes.Buffer
		cluster.Write(u(model.IDClusterTimecode, tc))
		cluster.Write(ebml.EncodeElement(model.IDSimpleBlock,
			simpleBlock(1, 0, 0x80, frameBytes(i+1, videoFrameBytes))))
		cluster.Write(ebml.EncodeElement(model.IDSimpleBlock,
			simpleBlock(2, 5, 0x00, frameBytes(i+101, audioFrameBytes))))
		if unknownCluster {
			body.Write(ebml.EncodeElementUnknown(model.IDCluster, cluster.Bytes(), 8))
		} else {
			body.Write(ebml.EncodeElement(model.IDCluster, cluster.Bytes()))
		}
	}
	return body.Bytes()
}

func wrapSegment(body []byte) []byte {
	var out bytes.Buffer
	out.Write(ebml.EncodeElement(model.IDEBMLHeader,
		[]byte{0x42, 0x82, 0x88, 'm', 'a', 't', 'r', 'o', 's', 'k', 'a'}))
	out.Write(ebml.EncodeElementUnknown(model.IDSegment, body, 8))
	return out.Bytes()
}

func generateStandard(blockCount int) Sample {
	return Sample{
		Name:       string(CaseStandard),
		Data:       wrapSegment(segmentBody(CaseStandard, blockCount, true)),
		BlockCount: blockCount * 2,
	}
}

func generateUnknownBlock(blockCount int) Sample {
	// Every cluster uses the unknown-length form; blocks stay known-size.
	return Sample{
		Name:       string(CaseUnknownBlock),
		Data:       wrapSegment(segmentBody(CaseUnknownBlock, blockCount, true)),
		BlockCount: blockCount * 2,
	}
}

func generateTruncated(blockCount int) Sample {
	good := wrapSegment(segmentBody(CaseTruncated, blockCount, false))
	// Append a 4-byte VINT marker whose remaining bytes are missing: the
	// parser must report its exact offset while earlier blocks survive.
	damaged := append(good, 0x10)
	return Sample{
		Name:       string(CaseTruncated),
		Data:       damaged,
		BlockCount: blockCount * 2,
	}
}

func generateChannelMismatch(blockCount int) Sample {
	return Sample{
		Name:          string(CaseChannelMismatch),
		Data:          wrapSegment(segmentBody(CaseChannelMismatch, blockCount, true)),
		BlockCount:    blockCount * 2,
		ExpectWarning: "AUDIO_CHANNEL_MISMATCH",
	}
}

func generateSampleRateConflict(blockCount int) Sample {
	return Sample{
		Name:          string(CaseSampleRateConflict),
		Data:          wrapSegment(segmentBody(CaseSampleRateConflict, blockCount, true)),
		BlockCount:    blockCount * 2,
		ExpectWarning: "AUDIO_SAMPLE_RATE_CONFLICT",
	}
}

func generateColorMissing(blockCount int) Sample {
	return Sample{
		Name:          string(CaseColorMissing),
		Data:          wrapSegment(segmentBody(CaseColorMissing, blockCount, true)),
		BlockCount:    blockCount * 2,
		ExpectWarning: "COLOR_METADATA_MISSING",
	}
}
