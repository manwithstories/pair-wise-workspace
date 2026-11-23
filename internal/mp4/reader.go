package mp4

import (
	"crypto/sha256"
	"fmt"
	"os"

	"github.com/abema/go-mp4"

	"mediasync/internal/model"
)

// ReadFile parses one MP4 input. It never writes.
func ReadFile(path string) (*Input, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return NewReader(f, st.Size()).Read(path)
}

// Read parses the whole input. Structural failures return an Input carrying a
// Truncation plus a coded error so callers can report both.
func (r *Reader) Read(path string) (*Input, error) {
	in := &Input{Path: path}
	top, err := r.boxesIn(0, uint64(r.size))
	if err != nil {
		return in, r.classify(err)
	}
	moov, ok := find(top, "moov")
	if !ok {
		return in, model.NewError(model.ErrTruncatedBox, 0, 0, "moov box not found")
	}
	mvhd, ok := find(top, "mvhd")
	_ = mvhd
	if !ok {
		// mvhd lives inside moov; not fatal for track import, but note it.
		in.FastStart = true
	}
	mdat, hasMdat := find(top, "mdat")
	if !hasMdat {
		return in, model.NewError(model.ErrTruncatedBox, 0, int64(moov.offset), "mdat box not found")
	}
	in.FastStart = moov.offset < mdat.offset

	mkids, err := r.boxesIn(moov.PayloadOffset(), moov.offset+moov.total)
	if err != nil {
		return in, r.classify(err)
	}
	mv, _ := find(mkids, "mvhd")
	if mv.typ != "" {
		// Touch mvhd so a malformed header is reported early.
		var m mp4.Mvhd
		if err := r.unmarshal(mv, &m, nil); err != nil {
			return in, err
		}
	}

	for _, tr := range findAll(mkids, "trak") {
		track, stats, terr := r.readTrak(tr, mdat)
		if terr != nil {
			return in, terr
		}
		in.Tracks = append(in.Tracks, track)
		in.Stats = append(in.Stats, stats)
	}
	if len(in.Tracks) == 0 {
		return in, model.NewError(model.ErrTruncatedBox, 0, 0, "no tracks found in moov")
	}
	return in, nil
}

func (r *Reader) readTrak(trak hdr, mdat hdr) (*model.Track, model.TrackStats, error) {
	kids, err := r.boxesIn(trak.PayloadOffset(), trak.offset+trak.total)
	if err != nil {
		return nil, model.TrackStats{}, r.classify(err)
	}
	tkhdH, ok := find(kids, "tkhd")
	if !ok {
		return nil, model.TrackStats{}, model.NewError(model.ErrTruncatedBox, 0, int64(trak.offset), "trak has no tkhd")
	}
	var tkhd mp4.Tkhd
	if err := r.unmarshal(tkhdH, &tkhd, nil); err != nil {
		return nil, model.TrackStats{}, err
	}
	trackID := tkhd.TrackID

	mdiaH, ok := find(kids, "mdia")
	if !ok {
		return nil, model.TrackStats{}, model.NewError(model.ErrTruncatedBox, trackID, int64(trak.offset), "trak has no mdia")
	}
	mdiaKids, err := r.boxesIn(mdiaH.PayloadOffset(), mdiaH.offset+mdiaH.total)
	if err != nil {
		return nil, model.TrackStats{}, r.classify(err)
	}
	mdhdH, ok := find(mdiaKids, "mdhd")
	if !ok {
		return nil, model.TrackStats{}, model.NewError(model.ErrTruncatedBox, trackID, int64(mdiaH.offset), "mdia has no mdhd")
	}
	var mdhd mp4.Mdhd
	if err := r.unmarshal(mdhdH, &mdhd, nil); err != nil {
		return nil, model.TrackStats{}, err
	}
	if mdhd.Timescale == 0 {
		return nil, model.TrackStats{}, model.NewError(model.ErrTruncatedBox, trackID, int64(mdhdH.offset), "mdhd timescale is zero")
	}

	hdlrH, ok := find(mdiaKids, "hdlr")
	if !ok {
		return nil, model.TrackStats{}, model.NewError(model.ErrTruncatedBox, trackID, int64(mdiaH.offset), "mdia has no hdlr")
	}
	var hdlr mp4.Hdlr
	if err := r.unmarshal(hdlrH, &hdlr, nil); err != nil {
		return nil, model.TrackStats{}, err
	}
	kind := kindForHandler(string(hdlr.HandlerType[:]))

	minfH, ok := find(mdiaKids, "minf")
	if !ok {
		return nil, model.TrackStats{}, model.NewError(model.ErrTruncatedBox, trackID, int64(mdiaH.offset), "mdia has no minf")
	}
	minfKids, err := r.boxesIn(minfH.PayloadOffset(), minfH.offset+minfH.total)
	if err != nil {
		return nil, model.TrackStats{}, r.classify(err)
	}
	stblH, ok := find(minfKids, "stbl")
	if !ok {
		return nil, model.TrackStats{}, model.NewError(model.ErrTruncatedBox, trackID, int64(minfH.offset), "minf has no stbl")
	}
	stblKids, err := r.boxesIn(stblH.PayloadOffset(), stblH.offset+stblH.total)
	if err != nil {
		return nil, model.TrackStats{}, r.classify(err)
	}

	tr := &model.Track{
		Source:    model.SourceMP4,
		ID:        trackID,
		Kind:      kind,
		Timescale: mdhd.Timescale,
		Meta:      model.DisplayMeta{SourceWidth: int(tkhd.GetWidth()), SourceHeight: int(tkhd.GetHeight())},
	}

	// Sample description: codec + display children.
	if stsdH, ok := find(stblKids, "stsd"); ok {
		if err := r.readStsd(stsdH, tr); err != nil {
			return nil, model.TrackStats{}, err
		}
	}

	// Matrix-derived rotation is a container-level claim.
	rot, okRot := rotationFromMatrix(tkhd.Matrix)
	if okRot {
		tr.Meta.Rotation = rot
		tr.Meta.RotationSrc = model.SourceMP4
		tr.Meta.Claims = append(tr.Meta.Claims, model.Claim{
			Field: "rotation", Value: fmt.Sprintf("%d", rot), Layer: model.LayerContainer, Source: model.SourceMP4,
		})
	} else {
		tr.Meta.Warnings = append(tr.Meta.Warnings, "tkhd matrix is not axis-aligned; rotation defaults to 0")
		tr.Meta.Claims = append(tr.Meta.Claims, model.Claim{
			Field: "rotation", Value: "0", Layer: model.LayerContainer, Source: model.SourceDefault,
			Reason: "unrepresentable matrix",
		})
	}

	sizes, err := r.readStsz(stblKids, trackID)
	if err != nil {
		return nil, model.TrackStats{}, err
	}
	durations, err := r.readStts(stblKids, trackID)
	if err != nil {
		return nil, model.TrackStats{}, err
	}
	chunks, perChunk, err := r.readChunks(stblKids, trackID)
	if err != nil {
		return nil, model.TrackStats{}, err
	}
	ctts, err := r.readCtts(stblKids, trackID)
	if err != nil {
		return nil, model.TrackStats{}, err
	}

	samples, err := buildSamples(trackID, sizes, durations, ctts, chunks, perChunk, uint64(r.size), mdat)
	if err != nil {
		return nil, model.TrackStats{}, err
	}
	tr.Samples = samples

	// Hash coded payloads in decode order.
	h := sha256.New()
	var total int64
	for i := range samples {
		s := &samples[i]
		if err := r.hashSample(h, s, trackID); err != nil {
			return nil, model.TrackStats{}, err
		}
		total += int64(s.Size)
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	stats := model.TrackStats{TrackID: trackID, Kind: kind, Samples: len(samples), Bytes: total, SHA256: digest}

	if tr.Width == 0 && tr.Meta.SourceWidth > 0 {
		tr.Width = tr.Meta.SourceWidth
		tr.Height = tr.Meta.SourceHeight
	}
	return tr, stats, nil
}

// hashSample hashes the coded bytes of a sample, verifying they fall inside the
// input first.
func (r *Reader) hashSample(h interface{ Write([]byte) (int, error) }, s *model.Sample, trackID uint32) error {
	if s.Size == 0 {
		return nil
	}
	if s.Size > maxSampleBytes {
		return model.NewError(model.ErrSizeLimit, trackID, s.Offset, "sample %d exceeds block limit", s.Index)
	}
	end := s.Offset + int64(s.Size)
	if end > r.size {
		return model.NewError(model.ErrTruncatedBox, trackID, s.Offset,
			"sample %d ends at %d past input size %d", s.Index, end, r.size)
	}
	buf := make([]byte, s.Size)
	if _, err := r.src.ReadAt(buf, s.Offset); err != nil {
		return model.WrapError(model.ErrTruncatedBox, trackID, s.Offset, err)
	}
	if _, err := h.Write(buf); err != nil {
		return err
	}
	return nil
}
