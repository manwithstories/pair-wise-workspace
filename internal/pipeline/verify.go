package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"

	"mediad/internal/bmff"
	"mediad/internal/model"
	"mediad/internal/trackmap"
)

type SampleDigest struct {
	OutputTrackID uint32 `json:"output_track_id"`
	SampleIndex   int    `json:"sample_index"`
	SHA256        string `json:"sha256"`
}

type LosslessReport struct {
	Verified      bool           `json:"verified"`
	Tracks        int            `json:"tracks"`
	Samples       int            `json:"samples"`
	SampleDigests []SampleDigest `json:"sample_digests"`
}

func VerifyLossless(inputPath, outputPath string, plan *trackmap.Plan) (*LosslessReport, error) {
	outIndex, err := bmff.ScanFile(outputPath)
	if err != nil {
		return nil, err
	}
	in, err := os.Open(inputPath)
	if err != nil {
		return nil, model.Wrap(model.CodeInvalidArgument, model.StagePipeline, "open source for verification", err)
	}
	defer in.Close()
	out, err := os.Open(outputPath)
	if err != nil {
		return nil, model.Wrap(model.CodeInvalidArgument, model.StagePipeline, "open output for verification", err)
	}
	defer out.Close()

	report := &LosslessReport{Verified: true, Tracks: len(plan.Tracks)}
	for _, pt := range plan.Tracks {
		ot, ok := outIndex.Track(pt.OutputID)
		if !ok {
			return nil, model.Newf(model.CodeIntegrity, model.StagePipeline, "output track %d missing after scan", pt.OutputID)
		}
		if len(ot.Samples) != len(pt.Samples) {
			return nil, model.Newf(model.CodeIntegrity, model.StagePipeline,
				"track %d sample count %d != %d", pt.OutputID, len(ot.Samples), len(pt.Samples))
		}
		for i, ps := range pt.Samples {
			osamp := ot.Samples[i]
			if osamp.Size != ps.Size {
				return nil, model.Newf(model.CodeIntegrity, model.StagePipeline, "track %d sample %d size changed", pt.OutputID, i)
			}
			srcSum, err := hashSection(in, ps.Offset, int64(ps.Size))
			if err != nil {
				return nil, err
			}
			outSum, err := hashSection(out, osamp.Offset, int64(osamp.Size))
			if err != nil {
				return nil, err
			}
			if srcSum != outSum {
				return nil, model.New(model.CodeIntegrity, model.StagePipeline, "sample SHA-256 differs").
					WithTrack(pt.OutputID).WithExpectedOffset(ps.Offset).WithDigest(srcSum, outSum)
			}
			outTime := model.Ticks(osamp.DTS, ot.Timescale)
			if outTime.Cmp(ps.DTSTime) != 0 {
				return nil, model.New(model.CodeTimestampBackward, model.StagePipeline, "output DTS rational time differs").
					WithTrack(pt.OutputID)
			}
			report.Samples++
			report.SampleDigests = append(report.SampleDigests, SampleDigest{
				OutputTrackID: pt.OutputID, SampleIndex: i, SHA256: srcSum,
			})
		}
	}
	return report, nil
}

func hashSection(f *os.File, off, size int64) (string, error) {
	h := sha256.New()
	if _, err := io.CopyN(h, io.NewSectionReader(f, off, size), size); err != nil {
		return "", model.Wrap(model.CodeIntegrity, model.StagePipeline, "hash sample range", err).WithOffset(off)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
