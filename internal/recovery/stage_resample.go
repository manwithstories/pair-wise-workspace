package recovery

import (
	"fmt"
	"os"
	"path/filepath"

	"mediad/internal/fault"
)

func (e *exec) stageResample() error {
	var pcmBytes []byte
	processed, reused := 0, 0

	for _, seg := range e.segs {
		name := fmt.Sprintf("segment-%02d.pcm16", seg.index+1)
		path := e.artifactPath(name)
		state, ok := e.cp.Segment(seg.hash)
		if !ok {
			return fault.New(fault.CheckpointCorrupt,
				filepath.Join(e.opts.Root, "work", "checkpoint.json"), -1,
				"missing state for segment %d", seg.index)
		}

		var payload []byte
		wantHash := state.ArtifactHashes["pcm16"]
		if stageIndex(state.CompletedStage) >= stageIndex("resample") && wantHash != "" {
			data, err := os.ReadFile(path)
			if err == nil && HashBytes(data) == wantHash {
				payload = data
				reused++
			}
		}
		if payload == nil {
			pcm, err := processPCM(seg.pcm, e.params)
			if err != nil {
				return err
			}
			payload = encodePCM16LE(pcm)
			if err := writeFileAtomic(path, payload); err != nil {
				return err
			}
			state.CompletedStage = "resample"
			if state.ArtifactHashes == nil {
				state.ArtifactHashes = map[string]string{}
			}
			state.ArtifactHashes["pcm16"] = HashBytes(payload)
			state.ArtifactHashes["pcm16_file"] = name
			e.cp.PutSegment(state)
			if err := e.cp.SaveAtomic(e.opts.Root); err != nil {
				return err
			}
			processed++
		}
		pcmBytes = append(pcmBytes, payload...)
	}

	e.pcm = pcmBytes
	e.sum.ProcessedSegments = processed
	e.sum.ReusedSegments = reused
	return e.finishStage("resample")
}
