package pipeline

import (
	"fmt"
	"os"

	"mediad/internal/ingest"
	"mediad/internal/model"
)

// reassemblePreview builds this track's deterministic preview stream, encodes
// it as real RTP packets through pion/rtp, decodes them back and reorders. The
// loss and gap numbers in the report therefore come from an actual
// marshal/unmarshal/reorder pass, and the preview is saved as an artifact.
func reassemblePreview(opt Options, paths Paths) (previewResult, error) {
	var out previewResult
	if opt.TargetFrames <= 0 {
		return out, nil
	}
	frames := opt.TargetFrames
	packets, err := ingest.BuildPreview(opt.TrackID, frames, int64(2000+opt.TrackID), opt.RTPLoss, 11, true)
	if err != nil {
		return out, model.Fatal("pipeline.rtp", err)
	}
	// Round-trip every packet through RTP so the reassembler sees wire bytes,
	// not in-memory structs.
	decoded := make([]ingest.PreviewPacket, 0, len(packets))
	for _, p := range packets {
		raw, err := ingest.EncodePreview(p, opt.TrackID)
		if err != nil {
			return out, model.Recoverable("pipeline.rtp", int(opt.TrackID), p.FrameNo, "encode", err)
		}
		d, err := ingest.DecodePreview(raw)
		if err != nil {
			return out, err
		}
		decoded = append(decoded, d)
	}
	st, err := ingest.Reorder(decoded)
	if err != nil {
		return out, model.Fatal("pipeline.rtp", err)
	}
	out.Lost = st.Lost
	out.Received = st.Received
	out.Missing = ingest.MissingFrames(st)
	// Persist a small preview report so the number is auditable.
	if err := writePreviewReport(paths, st); err != nil {
		return out, err
	}
	return out, nil
}

func writePreviewReport(paths Paths, st *ingest.RTPStats) error {
	path := paths.Preview()
	tmp := path + ".tmp"
	var b []byte
	b = append(b, fmt.Sprintf("received %d\n", st.Received)...)
	b = append(b, fmt.Sprintf("lost %d\n", st.Lost)...)
	b = append(b, fmt.Sprintf("duplicates %d\n", st.Duplicates)...)
	b = append(b, fmt.Sprintf("reordered %d\n", st.Reordered)...)
	b = append(b, fmt.Sprintf("complete_frames %d\n", len(st.Frames))...)
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return model.Fatal("pipeline.rtp", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return model.Fatal("pipeline.rtp", err)
	}
	return nil
}

// previewResult is what the RTP reassembly pass learned.
type previewResult struct {
	Lost     int      // missing sequence numbers
	Missing  []uint64 // frame numbers whose preview never completed
	Received int
}
