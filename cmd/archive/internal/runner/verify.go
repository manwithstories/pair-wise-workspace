package runner

import (
	"encoding/json"
	"fmt"
	"os"

	"mediaarchive/internal/demux"
	"mediaarchive/internal/manifest"
	"mediaarchive/internal/model"
)

// VerifyResult is the terminal verification outcome.
type VerifyResult struct {
	MatchPct   float64
	Compared   int
	Mismatches int
	Manifest   *manifest.Manifest
}

// Verify reopens source and remuxed output, re-hashes every kept packet and
// confirms the output is a byte-faithful, re-encode-free remux. It also
// re-checks the manifest's own claims.
func Verify(input, output, manifestPath string) (*VerifyResult, error) {
	src, err := demux.Open(input)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	dst, err := demux.Open(output)
	if err != nil {
		return nil, err
	}
	defer dst.Close()

	srcHash := map[string][32]byte{}
	err = src.StreamSamples(demux.StreamOptions{RecoverTruncated: true}, func(s demux.Sample) error {
		if s.Flags&demux.FlagTruncated != 0 {
			return nil
		}
		srcHash[hashKey(s.TrackID, s.Seq)] = sha256Bytes(s.Payload)
		return nil
	})
	if err != nil {
		return nil, err
	}

	compared, mismatches := 0, 0
	err = dst.StreamSamples(demux.StreamOptions{RecoverTruncated: false}, func(s demux.Sample) error {
		got := sha256Bytes(s.Payload)
		want, ok := srcHash[hashKey(s.TrackID, s.Seq)]
		if !ok {
			mismatches++
			return model.NewError(model.StageManifest,
				fmt.Sprintf("output packet has no source counterpart: track=%d seq=%d",
					s.TrackID, s.Seq)).WithTrack(int32(s.TrackID)).WithSample(s.Seq)
		}
		compared++
		if got != want {
			mismatches++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	mf, err := loadManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	match := 0.0
	if compared > 0 {
		match = float64(compared-mismatches) * 100 / float64(compared)
	}
	if mismatches > 0 || mf.SampleSHA256Match < 100 {
		return &VerifyResult{MatchPct: match, Compared: compared,
				Mismatches: mismatches, Manifest: mf},
			model.NewError(model.StageManifest,
				fmt.Sprintf("hash verification failed: %d/%d mismatches", mismatches, compared))
	}
	return &VerifyResult{MatchPct: match, Compared: compared, Mismatches: mismatches,
		Manifest: mf}, nil
}

func hashKey(trackID uint32, seq uint32) string {
	return fmt.Sprintf("%d:%d", trackID, seq)
}

func loadManifest(path string) (*manifest.Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, model.NewError(model.StageManifest, "cannot read manifest").Wrap(err)
	}
	var mf manifest.Manifest
	if err := json.Unmarshal(b, &mf); err != nil {
		return nil, model.NewError(model.StageManifest, "invalid manifest json").Wrap(err)
	}
	return &mf, nil
}
