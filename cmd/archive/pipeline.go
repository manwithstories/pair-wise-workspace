package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"archive/internal/errs"
	"archive/internal/index"
	"archive/internal/manifest"
	"archive/internal/mapping"
	"archive/internal/mp4"
	"archive/internal/remux"
	"archive/internal/report"
	"archive/internal/segment"
)

// assetInput locates one asset on disk.
type assetInput struct {
	name, kind, dir, manifestFile string
}

// outcome is the result of processing one asset.
type outcome struct {
	rep       report.AssetReport
	syncErrs  []float64
	errKind   string // errs.Kind when failed
	resumePos int    // >0 when processing resumed from a checkpoint
}

var segSeqRe = regexp.MustCompile(`^seg_0*([0-9]+)[^0-9].*\.m4s$|^seg_0*([0-9]+)\.m4s$`)

// processAsset runs the full pipeline for one asset: manifest parse,
// segment fetch/dedup/order, demux, remux with checkpointing, and output
// verification (payload hash + AV sync error).
func processAsset(ctx context.Context, a assetInput, outDir string, chunkBytes int64, rng *rand.Rand) outcome {
	start := time.Now()
	o := outcome{rep: report.AssetReport{Name: a.name, Kind: a.kind, Status: "ok"}}
	fail := func(err error) outcome {
		o.rep.Status = "failed"
		o.rep.Error = err.Error()
		if e, ok := err.(*errs.Error); ok {
			o.errKind = string(e.Kind)
		}
		o.rep.ProcessingMs = elapsedMs(start)
		return o
	}
	// 1. manifest (master playlists are resolved to their first variant)
	manifestBytes, err := os.ReadFile(filepath.Join(a.dir, a.manifestFile))
	if err != nil {
		return fail(errs.New(errs.KindManifest, a.name, a.manifestFile, "cannot read manifest", err))
	}
	mf, err := manifest.Parse(a.name, manifestBytes)
	if err != nil {
		return fail(err)
	}
	if mf.Type == manifest.TypeHLSMaster {
		if len(mf.Variants) == 0 {
			return fail(errs.New(errs.KindManifest, a.name, a.manifestFile, "master playlist has no variants", nil))
		}
		uri := mf.Variants[0].URI
		if err := manifest.ValidateURI(a.name, uri); err != nil {
			return fail(err)
		}
		mediaBytes, err := os.ReadFile(filepath.Join(a.dir, uri))
		if err != nil {
			return fail(errs.New(errs.KindSegment, a.name, uri, "cannot read variant playlist", err))
		}
		manifestBytes = append(manifestBytes, mediaBytes...)
		if mf, err = manifest.Parse(a.name, mediaBytes); err != nil {
			return fail(err)
		}
	}
	// 2. segment candidates: manifest refs plus migration leftovers whose
	// sequence number is recoverable from the file name
	type cand struct {
		idx int
		uri string
	}
	var cands []cand
	referenced := map[string]bool{}
	for _, ref := range mf.Segments {
		cands = append(cands, cand{ref.Index, ref.URI})
		referenced[ref.URI] = true
	}
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return fail(errs.New(errs.KindIO, a.name, a.dir, "cannot list asset directory", err))
	}
	for _, e := range entries {
		if e.IsDir() || referenced[e.Name()] {
			continue
		}
		if m := segSeqRe.FindStringSubmatch(e.Name()); m != nil {
			seq, _ := strconv.Atoi(m[1] + m[2])
			cands = append(cands, cand{seq, e.Name()})
		}
	}
	// simulate out-of-order arrival from legacy storage
	rng.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
	// 3. fetch, dedup, order, gap-check
	segs := make([]segment.Segment, 0, len(cands))
	for _, c := range cands {
		data, err := os.ReadFile(filepath.Join(a.dir, c.uri))
		if err != nil {
			return fail(errs.New(errs.KindSegment, a.name, fmt.Sprintf("segment %d (%s)", c.idx, c.uri),
				"manifest references missing segment", err))
		}
		segs = append(segs, segment.New(c.idx, c.uri, data))
	}
	firstIdx, expected := -1, -1
	if len(mf.Segments) > 0 {
		firstIdx, expected = mf.Segments[0].Index, len(mf.Segments)
	}
	ord, err := segment.Order(a.name, segs, firstIdx, expected)
	if err != nil {
		return fail(err)
	}
	o.rep.Segments = len(ord.Ordered)
	o.rep.Duplicates = ord.Duplicates
	o.rep.Conflicts = len(ord.Conflicts)
	o.rep.Gaps = ord.Gaps
	if len(ord.Gaps) > 0 {
		return fail(errs.Newf(errs.KindSegment, a.name, "order", "missing segments: %v", ord.Gaps))
	}
	// 4. demux every segment
	demuxed := make([][]mp4.Track, len(ord.Ordered))
	for i, s := range ord.Ordered {
		if ctx.Err() != nil {
			return fail(errs.New(errs.KindIO, a.name, s.URI, "cancelled", ctx.Err()))
		}
		tracks, err := mp4.Demux(a.name, s.URI, s.Data)
		if err != nil {
			return fail(err)
		}
		demuxed[i] = tracks
	}
	// 5. remux with checkpointing
	res, outPath, resumePos, err := remuxWithCheckpoint(a, demuxed, ord.Ordered, manifestBytes, outDir, chunkBytes)
	if err != nil {
		return fail(err)
	}
	o.resumePos = resumePos
	// 6. verify output: re-demux, compare payload hash against the full
	// input (valid for fresh and resumed runs alike), measure AV sync
	outBytes, err := os.ReadFile(outPath)
	if err != nil {
		return fail(errs.New(errs.KindIO, a.name, outPath, "cannot read output", err))
	}
	outTracks, err := mp4.Demux(a.name, filepath.Base(outPath), outBytes)
	if err != nil {
		return fail(err)
	}
	if payloadHash(outTracks) != expectedPayloadHash(demuxed) {
		return fail(errs.New(errs.KindRemux, a.name, "verify", "output payload hash mismatch", nil))
	}
	o.rep.Tracks = len(outTracks)
	o.rep.Samples = res.Samples
	o.syncErrs = syncErrors(outTracks)
	if len(o.syncErrs) > 0 {
		o.rep.AVSyncP95Ms = report.Percentile(o.syncErrs, 95)
	}
	o.rep.ProcessingMs = elapsedMs(start)
	return o
}

// remuxWithCheckpoint remuxes demuxed segments to outDir/<name>.mp4,
// committing a checkpoint every chunkBytes of output. On restart it
// verifies the index and committed chunks, truncates to the last valid
// checkpoint and resumes. Returns the remux result, output path and the
// segment position processing resumed from (0 = fresh run).
func remuxWithCheckpoint(a assetInput, demuxed [][]mp4.Track, ordered []segment.Segment,
	manifestBytes []byte, outDir string, chunkBytes int64) (*remux.Result, string, int, error) {
	ih := sha256.New()
	ih.Write(manifestBytes)
	for _, s := range ordered {
		ih.Write(s.Hash[:])
	}
	inputHash := hex.EncodeToString(ih.Sum(nil))
	outPath := filepath.Join(outDir, a.name+".mp4")
	idxPath := outPath + ".index.json"
	cp := index.New(a.name, inputHash, chunkBytes)
	resumePos, fresh := 0, true
	existing, loadErr := index.Load(idxPath)
	switch {
	case loadErr != nil:
		// corrupt index: nothing trustworthy, restart from checkpoint 0
	case existing != nil && existing.Verify(a.name, inputHash) == nil && existing.ChunkBytes == chunkBytes:
		f, err := os.Open(outPath)
		if err == nil {
			st, _ := f.Stat()
			valid := existing.FirstInvalidChunk(f, st.Size())
			f.Close()
			if valid == len(existing.Chunks) && st.Size() == existing.CoveredBytes() {
				// output complete and verified: nothing left to do
				return &remux.Result{
					Tracks: mapping.Map(demuxed[0]), BytesWritten: st.Size(),
					Samples: countSamples(demuxed),
				}, outPath, -1, nil
			}
			trunc, pos := existing.ResumePoint(valid)
			if valid > 0 && pos > 0 {
				existing.Chunks = existing.Chunks[:valid]
				if err := os.Truncate(outPath, trunc); err != nil {
					return nil, "", 0, errs.New(errs.KindIO, a.name, outPath, "cannot truncate output", err)
				}
				cp, resumePos, fresh = existing, pos, false
			}
			// valid == 0: no trustworthy checkpoint, fall back to a
			// fresh run (header must be rewritten)
		}
	}
	var out *os.File
	var err error
	if fresh {
		out, err = os.Create(outPath)
	} else {
		out, err = os.OpenFile(outPath, os.O_WRONLY, 0o644)
		if err == nil {
			_, err = out.Seek(0, 2) // append at the (truncated) end
		}
	}
	if err != nil {
		return nil, "", 0, errs.New(errs.KindIO, a.name, outPath, "cannot open output", err)
	}
	lastCommit := int64(0)
	if n := len(cp.Chunks); n > 0 {
		lastCommit = cp.Chunks[n-1].ByteEnd
	}
	commit := func(seg int, written int64, force bool) error {
		if !force && written-lastCommit < chunkBytes {
			return nil
		}
		rf, err := os.Open(outPath)
		if err != nil {
			return errs.New(errs.KindIO, a.name, outPath, "cannot hash output chunk", err)
		}
		h, err := index.HashRange(rf, lastCommit, written)
		rf.Close()
		if err != nil {
			return errs.New(errs.KindIndex, a.name, idxPath, "cannot hash output chunk", err)
		}
		cp.Chunks = append(cp.Chunks, index.Chunk{
			Index: len(cp.Chunks), ByteEnd: written, InputPos: seg + 1, SHA256: h,
		})
		lastCommit = written
		return index.Save(idxPath, cp)
	}
	res, err := remux.Remux(a.name, demuxed, out, remux.Options{
		SkipSegments: resumePos,
		StartSeq:     uint32(resumePos) + 1,
		OnFragment: func(seg int, written int64) error {
			return commit(seg, written, false)
		},
	})
	out.Close()
	if err != nil {
		return nil, "", 0, err
	}
	// commit the tail so the index always covers the whole file
	if res.BytesWritten > lastCommit {
		if err := commit(len(demuxed)-1, res.BytesWritten, true); err != nil {
			return nil, "", 0, err
		}
	}
	return res, outPath, resumePos, nil
}
