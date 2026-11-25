package pipeline

import (
	"fmt"
	"path/filepath"
	"sort"

	"mediqc/internal/audit"
	"mediqc/internal/checkpoint"
	"mediqc/internal/media"
	"mediqc/internal/remux"
)

func writeRemuxOutputs(outRoot, relDir string, cm *audit.CaseManifest, store *checkpoint.Store, tracks []media.Track, samples []media.Sample) *media.Error {
	tsBytes, merr := remux.BuildTS(tracks, samples, remux.TSOptions{})
	if merr != nil {
		return merr
	}
	tsRel := relDir + "/output.ts"
	if err := checkpoint.WriteFileAtomic(filepath.Join(outRoot, filepath.FromSlash(tsRel)), tsBytes, 0o644); err != nil {
		return err
	}
	cm.OutputTS = ptrDigest(digestFromBytes(tsRel, tsBytes))
	if err := commitChunk(cm, store, checkpoint.Chunk{
		ID: "output-ts-" + cm.CaseID, Index: 0, SHA256: audit.HashBytes(tsBytes), Bytes: len(tsBytes),
	}); err != nil {
		return err
	}

	fragments := buildFragments(samples, 8)
	fmp4Bytes, merr := remux.BuildFMP4(tracks, fragments)
	if merr != nil {
		return merr
	}
	mp4Rel := relDir + "/output.fmp4"
	if err := checkpoint.WriteFileAtomic(filepath.Join(outRoot, filepath.FromSlash(mp4Rel)), fmp4Bytes, 0o644); err != nil {
		return err
	}
	cm.OutputFMP4 = ptrDigest(digestFromBytes(mp4Rel, fmp4Bytes))
	if err := commitChunk(cm, store, checkpoint.Chunk{
		ID: "output-fmp4-" + cm.CaseID, Index: 1, SHA256: audit.HashBytes(fmp4Bytes), Bytes: len(fmp4Bytes),
	}); err != nil {
		return err
	}

	for i, frag := range fragments {
		payload := 0
		h := ""
		for _, s := range frag.Samples {
			payload += len(s.Data)
			h = s.Hash() // chunk identity below hashes concatenated payload deterministically
		}
		chunkBytes := make([]byte, 0, payload)
		for _, s := range frag.Samples {
			chunkBytes = append(chunkBytes, s.Data...)
		}
		if err := commitChunk(cm, store, checkpoint.Chunk{
			ID: fmt.Sprintf("fragment-%s-%03d", cm.CaseID, i), Index: i + 2,
			SHA256: audit.HashBytes(chunkBytes), Bytes: len(chunkBytes),
		}); err != nil {
			return err
		}
		_ = h
	}
	return nil
}

func buildFragments(samples []media.Sample, groupSize int) []remux.Fragment {
	if groupSize <= 0 {
		groupSize = 8
	}
	byTrack := map[int][]media.Sample{}
	for _, s := range samples {
		byTrack[s.TrackID] = append(byTrack[s.TrackID], s)
	}
	trackIDs := make([]int, 0, len(byTrack))
	for id := range byTrack {
		trackIDs = append(trackIDs, id)
	}
	sort.Ints(trackIDs)

	var out []remux.Fragment
	seq := uint32(1)
	for _, id := range trackIDs {
		items := byTrack[id]
		for start := 0; start < len(items); start += groupSize {
			end := start + groupSize
			if end > len(items) {
				end = len(items)
			}
			group := items[start:end]
			base := group[0].DTS
			if base < 0 {
				base = 0
			}
			out = append(out, remux.Fragment{
				Sequence: seq, TrackID: id, BaseDecodeTime: uint64(base), Samples: group,
			})
			seq++
		}
	}
	return out
}

func ptrDigest(d audit.FileDigest) *audit.FileDigest { return &d }

func buildTraceChain(cm *audit.CaseManifest, note string) []audit.TraceStage {
	traceID := cm.TraceID
	chain := []audit.TraceStage{}
	seen := map[media.Code]bool{}
	for _, e := range cm.Errors {
		if e == nil || seen[e.Code] {
			continue
		}
		switch e.Code {
		case media.CodeCRCInvalid, media.CodeCCGap, media.CodeClockWrap, media.CodeSyncLost, media.CodeUnexpectedEOF:
			seen[e.Code] = true
			chain = append(chain, audit.TraceStage{
				Component: e.Component, Code: e.Code, TraceID: traceID, Offset: e.Offset, Note: e.Message,
			})
		}
	}
	if cm.PSISwitches > 0 {
		chain = append(chain, audit.TraceStage{
			Component: "timeline.mapping", Code: media.CodeOK, TraceID: traceID,
			Note: "CRC-valid PMT version switch carried into track schedule",
		})
	}
	if cm.LateReplays > 0 {
		chain = append(chain, audit.TraceStage{
			Component: "timeline.replay", Code: media.CodeLateReplay, TraceID: traceID,
			Note: fmt.Sprintf("%d bounded deterministic replay(s); VFR intervals retained", cm.LateReplays),
		})
	}
	chain = append(chain, audit.TraceStage{
		Component: "remux", Code: media.CodeOK, TraceID: traceID, Note: note,
	})
	return chain
}
