package remux

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"

	"archivecheck/internal/demux"
	"archivecheck/internal/media"
)

// TrackInput is one recovered track to place in an output fragment.
type TrackInput struct {
	Meta    demux.TrackMeta
	Samples []demux.Sample
}

// Result describes one written archive fragment.
type Result struct {
	Path       string
	Size       int64
	SHA256     [32]byte
	Tracks     int
	Samples    int
	TrackCount map[int]int // samples per 1-based track
}

// WriteFragment serializes the tracks into a single MP4 at path inside the
// archive root. Coded sample bytes are copied to mdat unchanged. The file is
// written via a temp file and atomically renamed, so an interrupted run never
// leaves a half-written fragment under its final name.
func WriteFragment(path string, tracks []TrackInput) (*Result, error) {
	if len(tracks) == 0 {
		return nil, errors.New("remux: no tracks to write")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, media.NewFault(media.KindIO, filepath.Base(path), 0, -1, "mkdir: %s", err)
	}

	ftyp := buildFtyp()
	var head bytes.Buffer
	if _, err := ftyp.serialize(&head); err != nil {
		return nil, err
	}
	ftypLen := int64(head.Len())

	// Stage the mdat payload in memory (bounded by the same per-fragment cap
	// enforced upstream) while recording every sample's absolute file offset
	// and framed size, then build moov against those final offsets.
	hasher := newSha256()
	mdatPayload := bytes.NewBuffer(nil)
	perTrackOff := make([][]int64, len(tracks))
	perTrackSize := make([][]uint32, len(tracks))
	var totalSamples int
	moovProbeLen := int64(0)
	{
		var probe bytes.Buffer
		probeMoov := buildMoov(tracks, perTrackOff, perTrackSize, 0)
		_, _ = probeMoov.serialize(&probe)
		moovProbeLen = int64(probe.Len())
	}
	mdatStart := ftypLen + moovProbeLen
	payloadCursor := mdatStart + 8 // after mdat header
	for ti, tr := range tracks {
		perTrackOff[ti] = make([]int64, 0, 1)
		perTrackSize[ti] = make([]uint32, 0, len(tr.Samples))
		for si, s := range tr.Samples {
			l := make([]byte, 4)
			binary.BigEndian.PutUint32(l, uint32(len(s.Data)))
			mdatPayload.Write(l)
			mdatPayload.Write(s.Data)
			hasher.Write(l)
			hasher.Write(s.Data)
			if si == 0 {
				perTrackOff[ti] = append(perTrackOff[ti], payloadCursor) // chunk base
			}
			perTrackSize[ti] = append(perTrackSize[ti], uint32(4+len(s.Data)))
			payloadCursor += int64(4 + len(s.Data))
			totalSamples++
		}
	}
	moov := buildMoov(tracks, perTrackOff, perTrackSize, mdatStart+8)

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, media.NewFault(media.KindIO, filepath.Base(path), 0, -1, "create tmp: %s", err)
	}
	rollback := func() { _ = f.Close(); _ = os.Remove(tmp) }

	if _, err := ftyp.serialize(f); err != nil {
		rollback()
		return nil, err
	}
	if _, err := moov.serialize(f); err != nil {
		rollback()
		return nil, err
	}
	if err := writeMdat(f, mdatPayload.Bytes()); err != nil {
		rollback()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		rollback()
		return nil, media.NewFault(media.KindIO, filepath.Base(path), 0, -1, "fsync: %s", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, media.NewFault(media.KindIO, filepath.Base(path), 0, -1, "rename: %s", err)
	}

	fi, _ := os.Stat(path)
	res := &Result{
		Path: path, Size: fi.Size(), SHA256: hasher.Sum256(),
		Tracks: len(tracks), Samples: totalSamples, TrackCount: map[int]int{},
	}
	for _, tr := range tracks {
		res.TrackCount[tr.Meta.Track] = len(tr.Samples)
	}
	return res, nil
}

// writeMdat emits the mdat header then the verbatim payload.
func writeMdat(f *os.File, payload []byte) error {
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr, uint32(8+len(payload)))
	copy(hdr[4:8], "mdat")
	if _, err := f.Write(hdr); err != nil {
		return err
	}
	_, err := f.Write(payload)
	return err
}

// buildFtyp creates the file-type box (ISO base media, AVC-compatible).
func buildFtyp() boxBuilder {
	payload := make([]byte, 0, 20)
	payload = append(payload, []byte("isom")...)
	payload = append(payload, u32(0x200)...)
	payload = append(payload, []byte("isom")...)
	payload = append(payload, []byte("iso2")...)
	payload = append(payload, []byte("avc1")...)
	return *newBox("ftyp").setRaw(payload)
}
