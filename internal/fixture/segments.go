package fixture

import (
	"os"
	"path/filepath"
)

// segDir returns the segment directory for a representation role.
func segDir(camRoot, role string) string {
	return filepath.Join(camRoot, role)
}

// segPath returns the on-disk path of a segment file. Both HLS and DASH share
// the same encoded .ts payload under the role directory.
func segPath(camRoot, role string, seq int) string {
	return filepath.Join(segDir(camRoot, role), segmentFileName(seq))
}

// segmentFileName is the deterministic segment file name. It is unpadded so
// the same physical file backs both the HLS playlist and the DASH $Number$
// template without any name translation.
func segmentFileName(seq int) string {
	return "seg-" + itoaFixt(seq) + ".ts"
}

func itoaFixt(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// writeRepresentationSegments encodes every segment for one role and applies
// the file-level fault injections (missing file, truncation, colour tag).
func writeRepresentationSegments(camRoot, cam, role string, opt Options, plan *Plan, count int, segmentSec float64) error {
	dir := segDir(camRoot, role)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	isBackup := role == "backup"

	for seq := 0; seq < count; seq++ {
		// Lost primary slot: simply do not write the primary file.
		if !isBackup && plan.IsLost(cam, seq) {
			continue
		}
		// An unrecoverable slot is missing from the backup too.
		if isBackup && plan.IsUnrecoverable(cam, seq) {
			continue
		}

		spec := SegmentSpec{
			Width:        DefaultWidth,
			Height:       DefaultHeight,
			FrameRate:    DefaultFrameRate,
			SampleRate:   DefaultSampleRate,
			Channels:     DefaultChannels,
			DurationSec:  segmentSec,
			SegmentIndex: seq,
			// A real silence region gives the silence detector something to find.
			SilentAudio: seq == silenceSlot(cam),
		}
		// Colour conflict: the conflicting representation is tagged BT.2020.
		if plan.HasFault(cam, seq, "color_conflict") && isBackup {
			spec.ColorTag = ColorBT2020
		}

		data, err := EncodeSegment(spec)
		if err != nil {
			return err
		}

		// Truncation applies to the primary representation only; the backup
		// stays intact so the slot is recoverable.
		if !isBackup && plan.HasFault(cam, seq, "truncated") {
			data = truncatePacket(data)
		}
		if err := os.WriteFile(segPath(camRoot, role, seq), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// silenceSlot picks a deterministic slot per camera that carries genuine
// silent audio for the silence-detection stage.
func silenceSlot(cam string) int {
	switch cam {
	case "stage":
		return 6
	case "side":
		return 10
	default:
		return 14
	}
}

// truncatePacket cuts the payload part-way through and drops the tail so the
// muxer/demuxer sees an incomplete final packet. The TS sync header and the
// first whole packets are retained so the file still opens and probes.
func truncatePacket(data []byte) []byte {
	if len(data) <= 188*4 {
		return data
	}
	// Keep roughly 60% of the packets, ending on a TS packet boundary.
	keep := len(data) * 60 / 100
	keep = (keep / 188) * 188
	if keep < 188 {
		keep = 188
	}
	return data[:keep]
}
