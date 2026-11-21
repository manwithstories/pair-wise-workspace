package pipeline

import (
	"fmt"
	"io"
	"os"

	"mediad/internal/fixture"
	"mediad/internal/model"
	"mediad/internal/report"
)

func writeFixtures(seed int64, mp4Path, mxfPath string) error {
	mp4 := fixture.MP4Spec{Tracks: []fixture.TrackSpec{
		{
			ID: 1, Kind: "video", Codec: "avc1", Timescale: videoTimescale,
			Frames: videoFrames, FrameDelta: videoDelta,
			Width: 1920, Height: 1080,
			Rotation: 90, CropLeft: 4, CropRight: 4, SampleBytes: videoBytes, GOPLength: 30,
			Color: &fixture.ColorSpec{Primaries: 1, Transfer: 1, Matrix: 1},
		},
		{
			ID: 2, Kind: "audio", Codec: "mp4a", Timescale: audioTimescale,
			Frames: audioFrames, FrameDelta: audioDelta, SampleBytes: audioBytes,
		},
	}}
	if _, err := fixture.WriteMP4(mp4Path, mp4, seed); err != nil {
		return model.Wrap(model.KindInternal, "pipeline", "write mp4 fixture", err)
	}
	mxf := fixture.MXFSpec{Name: "studio camera", Tracks: []fixture.MXFTrack{
		{
			// The camera is track 3 in the unified timeline: MP4 video is 1
			// and MP4 audio is 2.
			TrackID: 3, Video: true, EditRateN: videoTimescale, EditRateD: videoDelta,
			EditUnits: videoFrames, StoredWidth: 1920, StoredHeight: 1080,
			Rotation: 270, HorizCrop: 4, EssenceBytes: mxfEssenceBytes,
			Color: &fixture.ColorSpec{Primaries: 1, Transfer: 1, Matrix: 1},
		},
	}}
	if _, err := fixture.WriteMXF(mxfPath, mxf, seed); err != nil {
		return model.Wrap(model.KindInternal, "pipeline", "write mxf fixture", err)
	}
	return nil
}

// measureQuality compares delivered sample bytes against the deterministic
// reference the fixture generator produced. Nothing is decoded: the metric is
// defined on the seeded payload pattern, so it is exactly reproducible.
func measureQuality(seed int64, c model.Container, paths map[string]string) (report.Quality, error) {
	q := report.Quality{Method: "deterministic seeded payload (no frame decode)"}
	files := map[string]*os.File{}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	var total float64
	var measured int
	for _, t := range c.Tracks {
		if len(t.Samples) == 0 {
			continue
		}
		path, ok := paths[t.Source]
		if !ok {
			continue
		}
		f := files[path]
		if f == nil {
			var err error
			if f, err = os.Open(path); err != nil {
				return q, model.Wrap(model.KindInternal, "pipeline", "open media for quality", err)
			}
			files[path] = f
		}
		for _, s := range t.Samples {
			if s.Damaged || s.Size == 0 {
				continue
			}
			got := make([]byte, s.Size)
			if _, err := f.ReadAt(got, s.Offset); err != nil && err != io.EOF {
				return q, model.Wrap(model.KindInvalidMedia, "pipeline",
					fmt.Sprintf("read sample %d of track %d", s.Index, t.ID), err)
			}
			want := fixture.SampleBytes(seed, t.ID, s.Index, int(s.Size))
			psnr, err := report.DeterministicPSNR(want, got)
			if err != nil {
				return q, err
			}
			total += psnr
			measured++
		}
	}
	q.Measured = measured
	if measured > 0 {
		q.PSNRdB = total / float64(measured)
	}
	return q, nil
}
