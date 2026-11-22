package export

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"medpipe/internal/fault"
	"medpipe/internal/merge"
	rtpstream "medpipe/internal/rtp"
)

// segmentWriter accumulates frames into fixed-size segments, writing each
// segment atomically (temp + rename) and checkpointed before the next
// one starts, so an interruption always leaves a valid boundary.
type segmentWriter struct {
	params    RunParams
	inputSHA  string
	buf       []byte
	records   []FrameRecord
	nextIndex uint64
	segments  []Segment
	offset    int64 // byte offset of the next segment inside frames.bin
	reuse     map[int]Segment
	reused    int
}

func newSegmentWriter(params RunParams, inputSHA string) *segmentWriter {
	return &segmentWriter{params: params, inputSHA: inputSHA, reuse: map[int]Segment{}}
}

func (sw *segmentWriter) segDir() string {
	return filepath.Join(sw.params.OutDir, "segments")
}

func (sw *segmentWriter) add(ev *merge.Event) error {
	frame := EncodeFrame(sw.nextIndex, ev)
	rec := NewFrameRecord(sw.nextIndex, ev, &frame)
	sw.buf = append(sw.buf, frame[:]...)
	sw.records = append(sw.records, rec)
	sw.nextIndex++
	if sw.offset+int64(len(sw.buf)) > sw.params.MaxOutBytes {
		return fault.New(fault.KindResource, int64(ev.Track), int64(ev.ExtSeq),
			ev.Offset, false, "output space limit exceeded")
	}
	if len(sw.records) >= FramesPerSeg {
		return sw.closeSegment()
	}
	return nil
}

// closeSegment finishes the current segment: hash, write (unless a
// verified identical segment already exists), checkpoint.
func (sw *segmentWriter) closeSegment() error {
	if len(sw.records) == 0 {
		return nil
	}
	idx := len(sw.segments)
	sum := sha256.Sum256(sw.buf)
	var jsonl []byte
	for _, r := range sw.records {
		jsonl = append(jsonl, MarshalFrameRecord(r)...)
	}
	jsum := sha256.Sum256(jsonl)
	seg := Segment{
		Index:   idx,
		Frames:  len(sw.records),
		Bytes:   int64(len(sw.buf)),
		Offset:  sw.offset,
		SHA256:  hex.EncodeToString(sum[:]),
		JSHA256: hex.EncodeToString(jsum[:]),
	}
	if r, ok := sw.reuse[idx]; ok && r.SHA256 == seg.SHA256 && r.Frames == seg.Frames {
		sw.reused++ // verified identical segment already on disk
	} else {
		base := filepath.Join(sw.segDir(), fmt.Sprintf("seg-%05d", idx))
		if err := writeFileAtomic(base+".bin", sw.buf); err != nil {
			return err
		}
		if err := writeFileAtomic(base+".jsonl", jsonl); err != nil {
			return err
		}
	}
	sw.segments = append(sw.segments, seg)
	sw.offset += seg.Bytes
	sw.buf = sw.buf[:0]
	sw.records = nil

	// Checkpoint after every segment: input digest, params digest,
	// completed segments and output offsets.
	cp := &Checkpoint{
		InputSHA256:  sw.inputSHA,
		ParamsSHA256: sw.params.digest(),
		Segments:     sw.segments,
	}
	return SaveCheckpoint(sw.params.CheckpointPath, cp)
}

// close flushes the trailing partial segment.
func (sw *segmentWriter) close() error { return sw.closeSegment() }

// finalize assembles the reviewable artifacts and the manifest, then
// completes the checkpoint. Segment files are the source of truth;
// frames.bin and provenance.jsonl are deterministic concatenations.
func (sw *segmentWriter) finalize(params RunParams, merger *merge.Merger,
	parser *rtpstream.Parser, inputSHA string) (*Metrics, error) {
	out := params.OutDir
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}

	// frames.bin = concat(segment bins)
	framesPath := filepath.Join(out, "frames.bin")
	if err := concatAtomic(framesPath, sw.segmentPaths(".bin")); err != nil {
		return nil, err
	}

	// provenance.jsonl = header + concat(segment jsonl)
	header := MarshalHeader(ManifestHeader{
		Type:         "header",
		InputSHA256:  inputSHA,
		ParamsSHA256: params.digest(),
		Anomalies:    AnomaliesFromFaults(merger.Anomalies),
	})
	provPath := filepath.Join(out, "provenance.jsonl")
	if err := concatAtomic(provPath, sw.segmentPaths(".jsonl"), header); err != nil {
		return nil, err
	}

	// Track timing and metrics from the merger's per-track stats.
	var timing []TrackTiming
	var packetsTotal, packetsLost, frames int
	for _, st := range merger.Stats() {
		sent := st.Sent
		if sent == 0 {
			sent = st.Received
		}
		timing = append(timing, TrackTiming{
			TrackID:       st.Track,
			Timescale:     rtpstream.ClockRate,
			DurationTicks: sent * rtpstream.TicksPerPacket,
			Frames:        st.Received,
		})
		packetsTotal += int(sent)
		packetsLost += int(st.Missing + st.TailLoss)
		frames += int(st.Received)
	}
	sort.Slice(timing, func(i, j int) bool { return timing[i].TrackID < timing[j].TrackID })

	// media.mp4 embeds the frame sequence with per-track timing.
	framesData, err := os.Open(framesPath)
	if err != nil {
		return nil, err
	}
	defer framesData.Close()
	if err := WriteMP4(filepath.Join(out, "media.mp4"), timing, framesData); err != nil {
		return nil, err
	}

	m := &Metrics{
		PacketsTotal:   packetsTotal,
		PacketsLost:    packetsLost,
		BadOffsetBytes: parser.Stats.BadOffsetBytes,
		FramesWritten:  frames,
	}

	// Output hashes over every artifact except manifest.json (which
	// carries run-dependent timing fields).
	outputs, err := hashOutputs(out)
	if err != nil {
		return nil, err
	}
	m.OutputSHA256 = outputDigest(outputs)

	manifest := &Manifest{
		InputSHA256:  inputSHA,
		ParamsSHA256: params.digest(),
		Segments:     sw.segments,
		Outputs:      outputs,
		Metrics:      *m,
		TrackTiming:  timing,
	}
	mdata, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(out, "manifest.json"), mdata); err != nil {
		return nil, err
	}

	// Complete the checkpoint with final metrics and output hashes.
	cp := &Checkpoint{
		InputSHA256:  inputSHA,
		ParamsSHA256: params.digest(),
		Segments:     sw.segments,
		Complete:     true,
		Metrics:      *m,
		Outputs:      outputs,
	}
	if err := SaveCheckpoint(params.CheckpointPath, cp); err != nil {
		return nil, err
	}
	return m, nil
}

func (sw *segmentWriter) segmentPaths(ext string) []string {
	var paths []string
	for _, seg := range sw.segments {
		paths = append(paths, filepath.Join(sw.segDir(),
			fmt.Sprintf("seg-%05d%s", seg.Index, ext)))
	}
	return paths
}

// concatAtomic writes the concatenation of paths (plus any leading
// bytes) to dst via temp+rename.
func concatAtomic(dst string, paths []string, leading ...[]byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".cat-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	for _, lead := range leading {
		if _, err := tmp.Write(lead); err != nil {
			tmp.Close()
			return err
		}
	}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			tmp.Close()
			return err
		}
		_, err = io.Copy(tmp, f)
		f.Close()
		if err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// hashOutputs hashes every artifact file under out except manifest.json.
func hashOutputs(out string) (map[string]string, error) {
	outputs := map[string]string{}
	root := filepath.Clean(out)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasPrefix(info.Name(), ".") {
			return nil // leftover temp file from an interrupted write
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "manifest.json" {
			return nil
		}
		sum, err := hashFile(path)
		if err != nil {
			return err
		}
		outputs[rel] = sum
		return nil
	})
	return outputs, err
}

// outputDigest reduces the output set to one deterministic hash.
func outputDigest(outputs map[string]string) string {
	keys := make([]string, 0, len(outputs))
	for k := range outputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s:%s\n", k, outputs[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func joinPath(dir, rel string) string { return filepath.Join(dir, rel) }

// segBinRel is the manifest-relative path of one segment's frame file.
func segBinRel(index int) string {
	return filepath.Join("segments", fmt.Sprintf("seg-%05d.bin", index))
}

// segJSONLRel is the manifest-relative path of one segment's provenance.
func segJSONLRel(index int) string {
	return filepath.Join("segments", fmt.Sprintf("seg-%05d.jsonl", index))
}

// cleanArtifacts removes artifacts of a previous run so a fresh full
// run never mixes stale files into the output set.
func cleanArtifacts(out string) {
	_ = os.RemoveAll(filepath.Join(out, "segments"))
	for _, f := range []string{"frames.bin", "provenance.jsonl", "media.mp4", "manifest.json"} {
		_ = os.Remove(filepath.Join(out, f))
	}
}
