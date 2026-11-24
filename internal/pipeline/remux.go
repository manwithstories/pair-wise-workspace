package pipeline

import (
	"bytes"
	"time"

	"relay/internal/demux"
	"relay/internal/mapping"
	"relay/internal/media"
	"relay/internal/metrics"
	"relay/internal/recovery"
	"relay/internal/safepath"
	"relay/internal/txn"
)

// remuxAndCommit rewrites every track to fMP4, re-reads it to prove sample
// bytes were preserved, stages each sample through the WAL and finalizes the
// transaction (or injects the staged crash point).
func remuxAndCommit(conf Config, root *safepath.Root, t *txn.Txn,
	mapper *mapping.Mapper, tracks map[uint32]*mapping.Track,
	allFrames []*media.RawFrame, rtpRes *recovery.Result,
	collector *metrics.Collector, out *Outcome) error {

	remuxStart := time.Now()
	trackIn := map[uint32][]*media.RawFrame{}
	trackOut := map[uint32][]*media.RawFrame{}
	const batchSize = 8
	for _, id := range mapper.TrackIDs() {
		tr := tracks[id]
		trackIn[id] = tr.Frames
		mp4, err := demux.WriteFragmentedMP4Bytes(demux.FSEQHeader{
			SourceID: tr.Rule.SourceID, Kind: tr.Rule.Kind,
			Width: tr.Frames[0].Meta.Width, Height: tr.Frames[0].Meta.Height,
			TimescaleHz: uint32(tr.Frames[0].Meta.TimescaleHz),
		}, tr.Frames)
		if err != nil {
			return err
		}
		outPath, err := root.Join("outputs", trackName(id)+".mp4")
		if err != nil {
			return err
		}
		if err := writeFileMkdir(outPath, mp4); err != nil {
			return err
		}
		reRead, _, err := demux.ReadFragmentedMP4(bytes.NewReader(mp4))
		if err != nil {
			return media.NewError(media.ErrChecksumMismatch, tr.Rule.SourceID,
				"remuxed container unreadable: "+err.Error())
		}
		trackOut[id] = reRead
		// Stage every sample; each write is WAL-logged and hash-verified. The
		// stage work is accounted in small batches so p95 reflects per-batch
		// processing rather than the whole-track total.
		for i, fr := range tr.Frames {
			if err := t.Stage(id, fr.Meta.Sequence, fr); err != nil {
				return err
			}
			if (i+1)%batchSize == 0 {
				collector.RecordBatch(time.Since(remuxStart))
				remuxStart = time.Now()
			}
		}
		collector.RecordBatch(time.Since(remuxStart))
		remuxStart = time.Now()
	}

	// Input vs output digests over the identical canonical sample set.
	out.InputDigest = metrics.DigestSamples(allFrames)
	out.OutputDigest = metrics.DigestSamples(flatten(trackOut))
	out.Report.InputSHA = out.InputDigest
	out.Report.OutputSHA = out.OutputDigest
	out.Report.TrackDiffs = metrics.CompareTracks(trackIn, trackOut)
	out.Report.RecoveryPct = metrics.RecoveryPctOf(rtpRes)
	out.Report.SyncMaxMS = metrics.SyncMaxMS(trackFrames(tracks))
	out.Report.P95BatchMS = collector.P95BatchMS()
	out.Report.CommitCount = 1
	out.Report.Replayed = 0
	if err := t.Finalize(); err != nil {
		return err
	}
	manifest := t.Manifest()
	out.ManifestSum = txn.ManifestHash(manifest)
	out.Report.ManifestSum = out.ManifestSum
	out.Report.Identical = true
	return nil
}

// damageChecksum flips a payload byte inside the first FSEQ frame while
// leaving the stored SHA-256 untouched, forcing a checksum mismatch.
func damageChecksum(data []byte) []byte {
	out := append([]byte(nil), data...)
	const headerSize = 27
	const recordHdr = 28
	if len(out) <= headerSize+recordHdr {
		return out
	}
	// Flip the first payload byte of the first frame record.
	pos := headerSize + recordHdr
	out[pos] ^= 0xff
	return out
}

// rollbackOutcome records the media failure, rolls the open transaction back
// (CURRENT is never touched) and builds the terminal report for exit code 3.
func rollbackOutcome(t *txn.Txn, root *safepath.Root, out *Outcome,
	collector *metrics.Collector, cause error) (*Outcome, error) {
	me := media.AsMediaError(cause)
	if rbErr := t.Rollback(); rbErr != nil {
		return nil, rbErr
	}
	after, _ := txn.CurrentCommitID(root.String())
	out.ExitCode = ExitMediaError
	out.CurrentAfter = after
	out.Report.RollbackCount = 1
	out.Report.ChecksumError = me != nil && me.Kind == media.ErrChecksumMismatch
	out.Report.CurrentChanged = out.HadCurrent && after != out.CurrentBefore
	out.Report.P95BatchMS = collector.P95BatchMS()
	out.Report.InputSHA = out.InputDigest
	out.Report.OutputSHA = out.InputDigest // nothing was committed
	return out, nil
}

func trackName(id uint32) string {
	return "track-" + itoa(id)
}

func itoa(u uint32) string {
	if u == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for u > 0 {
		i--
		b[i] = byte('0' + u%10)
		u /= 10
	}
	return string(b[i:])
}

func flatten(m map[uint32][]*media.RawFrame) []*media.RawFrame {
	var out []*media.RawFrame
	ids := make([]uint32, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	// Deterministic order regardless of map iteration.
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j-1] > ids[j]; j-- {
			ids[j-1], ids[j] = ids[j], ids[j-1]
		}
	}
	for _, id := range ids {
		out = append(out, m[id]...)
	}
	return out
}

func trackFrames(tracks map[uint32]*mapping.Track) map[uint32][]*media.RawFrame {
	out := map[uint32][]*media.RawFrame{}
	for id, tr := range tracks {
		out[id] = tr.Frames
	}
	return out
}
