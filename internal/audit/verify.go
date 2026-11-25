package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mediqc/internal/media"
	"mediqc/internal/ts"
)

// VerifyDirectory independently rereads the manifest and every referenced product.
func VerifyDirectory(dir string) (*VerifiedSuite, *media.Error) {
	manifestPath := filepath.Join(dir, "suite-manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, &media.Error{Code: media.CodeUnexpectedEOF, Component: "audit.verify", Message: err.Error()}
	}
	hash := HashBytes(raw)
	var suite SuiteManifest
	if err := json.Unmarshal(raw, &suite); err != nil {
		return nil, &media.Error{Code: media.CodeLengthInvalid, Component: "audit.verify", Message: err.Error()}
	}

	var matched, total int
	var maxSkew int64
	for i := range suite.CasesManifests {
		cm := &suite.CasesManifests[i]
		if verr := verifyDigest(dir, cm.Input); verr != nil {
			return nil, verr
		}
		if cm.RTPInput != nil {
			if verr := verifyDigest(dir, *cm.RTPInput); verr != nil {
				return nil, verr
			}
		}
		if verr := verifyDigest(dir, cm.Checkpoint); verr != nil {
			return nil, verr
		}
		if cm.Class == "rejected" {
			continue
		}
		if cm.OutputTS == nil || cm.OutputFMP4 == nil {
			return nil, &media.Error{Code: media.CodeLengthInvalid, Component: "audit.verify", Message: cm.CaseID + " missing remux outputs"}
		}
		if verr := verifyDigest(dir, *cm.OutputTS); verr != nil {
			return nil, verr
		}
		if verr := verifyDigest(dir, *cm.OutputFMP4); verr != nil {
			return nil, verr
		}
		bitrate := ComputeBitrate(cm.TargetBitrateBPS, cm.OutputTS.Bytes, cm.DurationMS)
		if bitrate.ActualBPS != cm.ActualBitrateBPS || abs64(bitrate.DeltaPPM) > 5000 {
			return nil, &media.Error{
				Code: media.CodeCountInvalid, Component: "audit.verify",
				Message: fmt.Sprintf("%s bitrate outside target: report=%+v", cm.CaseID, bitrate),
			}
		}

		inputHashes, inputCount, verr := readTSHashes(filepath.Join(dir, cm.Input.Path))
		if verr != nil {
			return nil, verr
		}
		tsHashes, tsCount, verr := readTSHashes(filepath.Join(dir, cm.OutputTS.Path))
		if verr != nil {
			return nil, verr
		}
		fmp4Hashes, _, verr := readFMP4Hashes(filepath.Join(dir, cm.OutputFMP4.Path))
		if verr != nil {
			return nil, verr
		}
		if inputCount != len(cm.Samples) || tsCount != len(cm.Samples) || len(fmp4Hashes) != len(cm.Samples) {
			return nil, &media.Error{
				Code: media.CodeCountInvalid, Component: "audit.verify",
				Message: fmt.Sprintf("%s sample counts manifest=%d input=%d ts=%d fmp4=%d", cm.CaseID, len(cm.Samples), inputCount, tsCount, len(fmp4Hashes)),
			}
		}

		total += 3 * len(cm.Samples)
		for _, ref := range cm.Samples {
			if consumeHash(inputHashes, ref.SHA256) {
				matched++
			}
			if consumeHash(tsHashes, ref.SHA256) {
				matched++
			}
			if consumeHash(fmp4Hashes, ref.SHA256) {
				matched++
			}
		}
		if matched < total {
			return nil, &media.Error{Code: media.CodeHashMismatch, Component: "audit.verify", Message: cm.CaseID + " sample hash mismatch"}
		}

		outSamples, verr := demuxTSFile(filepath.Join(dir, cm.OutputTS.Path))
		if verr != nil {
			return nil, verr
		}
		auditedDuration, skew := DurationSkew(cm.DurationMS, outSamples)
		cm.AuditedDurationMS = auditedDuration
		if skew > maxSkew {
			maxSkew = skew
		}
		if skew > 1 {
			return nil, &media.Error{Code: media.CodeManifestSkew, Component: "audit.verify", Message: fmt.Sprintf("%s skew %dms", cm.CaseID, skew)}
		}
		if !cm.ReplayHashEqual || cm.TimelineHash != cm.ReplayHash {
			return nil, &media.Error{Code: media.CodeHashMismatch, Component: "audit.verify", Message: cm.CaseID + " replay hash is not deterministic"}
		}
	}

	if total == 0 || matched != total {
		return nil, &media.Error{Code: media.CodeHashMismatch, Component: "audit.verify", Message: "not all samples were verified"}
	}
	if suite.SampleHashMatch != "100%" || !suite.ReplayHashEqual || suite.ManifestSkewMS > 1 {
		return nil, &media.Error{Code: media.CodeHashMismatch, Component: "audit.verify", Message: "suite summary failed independent verification"}
	}
	if maxSkew > 1 {
		return nil, &media.Error{Code: media.CodeManifestSkew, Component: "audit.verify", Message: fmt.Sprintf("manifest skew %dms", maxSkew)}
	}
	return &VerifiedSuite{Manifest: &suite, ManifestHash: hash, ManifestPath: manifestPath}, nil
}

func verifyDigest(dir string, d FileDigest) *media.Error {
	if d.Path == "" {
		return &media.Error{Code: media.CodeLengthInvalid, Component: "audit.verify", Message: "empty digest path"}
	}
	n, h, err := HashFile(filepath.Join(dir, d.Path))
	if err != nil {
		return &media.Error{Code: media.CodeUnexpectedEOF, Component: "audit.verify", Message: err.Error()}
	}
	if n != d.Bytes || h != d.SHA256 {
		return &media.Error{Code: media.CodeHashMismatch, Component: "audit.verify", Message: "digest mismatch for " + d.Path}
	}
	return nil
}

func demuxTSFile(path string) ([]media.Sample, *media.Error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, &media.Error{Code: media.CodeUnexpectedEOF, Component: "audit.verify", Message: err.Error()}
	}
	collector := media.NewCollector(256)
	d := ts.NewDemuxer(media.DefaultLimits(), collector)
	if ferr := d.Feed(b); ferr != nil && ferr.Fatal {
		return nil, ferr
	}
	if ferr := d.FlushPES(); ferr != nil {
		return nil, ferr
	}
	return d.Result().Samples, nil
}

func readTSHashes(path string) (map[string]int, int, *media.Error) {
	samples, err := demuxTSFile(path)
	if err != nil {
		return nil, 0, err
	}
	m := map[string]int{}
	for _, s := range samples {
		m[s.Hash()]++
	}
	return m, len(samples), nil
}

func readFMP4Hashes(path string) (map[string]int, int, *media.Error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, &media.Error{Code: media.CodeUnexpectedEOF, Component: "audit.verify", Message: err.Error()}
	}
	samples, merr := ExtractFMP4Samples(b)
	if merr != nil {
		return nil, 0, merr
	}
	m := map[string]int{}
	for _, s := range samples {
		sample := media.Sample{Data: s.Data}
		m[sample.Hash()]++
	}
	return m, len(samples), nil
}

func consumeHash(m map[string]int, h string) bool {
	if m[h] <= 0 {
		return false
	}
	m[h]--
	return true
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
