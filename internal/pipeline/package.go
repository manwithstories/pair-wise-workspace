package pipeline

import (
	"os"
	"path/filepath"

	"fieldcut/internal/audio"
	"fieldcut/internal/clock"
	"fieldcut/internal/cut"
	"fieldcut/internal/events"
	"fieldcut/internal/ingest"
	"fieldcut/internal/manifest"
	"fieldcut/internal/recover"
)

type packageResult struct {
	Segments     []manifest.Segment
	ActualBPS    float64
	BitrateDelta float64
}

func buildPackage(director, stage *ingest.MP4Track, pcm *audio.PCM, segs []clock.Interval, assigns []events.Assignment, j *recover.Journal, hashes map[string]string) (*packageResult, error) {
	outDir := filepath.Dir(j.Path)
	segDir := filepath.Join(outDir, "segments")
	trackDir := filepath.Join(outDir, "tracks")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(trackDir, 0o755); err != nil {
		return nil, err
	}
	committed := j.IsCommitted("package", hashes["package"])
	out := &packageResult{}
	outputHashes := map[string]string{}
	var videoBytes int

	for i, iv := range segs {
		ds := samplesInSegment(director, iv)
		if len(ds) == 0 {
			return nil, clock.Errorf(clock.CodeCoverage, director.TrackID, "segment %d has no director samples", i)
		}
		videoBlob, err := cut.PackageFragment(director.TrackID, director.TimeScale, director.Color, ds)
		if err != nil {
			return nil, err
		}
		ss := samplesInSegment(stage, iv)
		var stageBlob []byte
		if len(ss) > 0 {
			stageBlob, err = cut.PackageFragment(stage.TrackID, stage.TimeScale, stage.Color, ss)
			if err != nil {
				return nil, err
			}
		}
		audioBlob, err := slicePCM16(pcm, iv)
		if err != nil {
			return nil, err
		}

		videoBytes += len(videoBlob)
		relVideo := filepath.ToSlash(filepath.Join("segments", segmentName(i, "mp4")))
		relAudio := filepath.ToSlash(filepath.Join("segments", segmentName(i, "pcm")))
		relStage := filepath.ToSlash(filepath.Join("tracks", "stage-"+segmentName(i, "mp4")))
		videoSHA := HashBytes(videoBlob)
		audioSHA := HashBytes(audioBlob)
		outputHashes[relVideo] = videoSHA
		outputHashes[relAudio] = audioSHA
		if stageBlob != nil {
			outputHashes[relStage] = HashBytes(stageBlob)
		}

		if !committed {
			if err := recover.AtomicWriteFile(filepath.Join(outDir, relVideo), videoBlob); err != nil {
				return nil, err
			}
			if err := recover.AtomicWriteFile(filepath.Join(outDir, relAudio), audioBlob); err != nil {
				return nil, err
			}
			if stageBlob != nil {
				if err := recover.AtomicWriteFile(filepath.Join(outDir, relStage), stageBlob); err != nil {
					return nil, err
				}
			}
		}

		eventIDs := eventIDsForSegment(assigns, i)
		out.Segments = append(out.Segments, manifest.Segment{
			Index: i, File: relVideo, AudioFile: relAudio, Interval: iv,
			SHA256: videoSHA, AudioSHA256: audioSHA, SizeBytes: int64(len(videoBlob)),
			RandomAccess: true, EventIDs: eventIDs,
			Assignments: assignmentsForSegment(assigns, i),
		})
	}

	out.ActualBPS = float64(videoBytes) * 8 / (float64(Duration) / float64(clock.NsPerSecond))
	out.BitrateDelta = (out.ActualBPS - float64(TargetBPS)) / float64(TargetBPS) * 100
	if out.BitrateDelta > 5 || out.BitrateDelta < -5 {
		return nil, &clock.Error{Code: clock.CodeBitrate, TrackID: director.TrackID,
			OriginalValue: fmtBitrate(out.ActualBPS, out.BitrateDelta),
			Message:       "actual bitrate deviates more than 5% from target"}
	}
	if !committed {
		if err := j.Commit("package", hashes["package"], outputHashes); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func segmentName(i int, ext string) string {
	return fmtSegmentName(i) + "." + ext
}

func slicePCM16(p *audio.PCM, iv clock.Interval) ([]byte, error) {
	start := int(int64(iv.Start) * int64(p.SampleRate) / int64(clock.NsPerSecond))
	end := int(int64(iv.End) * int64(p.SampleRate) / int64(clock.NsPerSecond))
	if start < 0 || end < start {
		return nil, clock.Errorf(clock.CodeBadInterval, p.TrackID, "bad PCM slice %+v", iv)
	}
	channels := make([][]float64, len(p.Channels))
	for c, ch := range p.Channels {
		if end > len(ch) {
			end = len(ch)
		}
		if start > end {
			start = end
		}
		channels[c] = ch[start:end]
	}
	slice := &audio.PCM{TrackID: p.TrackID, SampleRate: p.SampleRate, Layout: p.Layout, Channels: channels}
	return audio.AppendPCM16(slice), nil
}

func eventIDsForSegment(a []events.Assignment, idx int) []string {
	seen := map[string]bool{}
	var ids []string
	for _, x := range a {
		if x.SegmentIndex == idx && !seen[x.EventID] {
			seen[x.EventID] = true
			ids = append(ids, x.EventID)
		}
	}
	return ids
}

func assignmentsForSegment(a []events.Assignment, idx int) []events.Assignment {
	var out []events.Assignment
	for _, x := range a {
		if x.SegmentIndex == idx {
			out = append(out, x)
		}
	}
	return out
}
