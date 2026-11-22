package scan

import (
	"encoding/binary"
	"os"

	"github.com/pion/rtp"

	"mediaslice/media"
)

// attachRTP streams the length-prefixed RTP sidecar, unwraps the 32-bit
// timestamps starting at the video track's edit-list media start, applies
// drift correction against the MP4 sample timing and records the stats on
// the video track. An unprovable wrap stops only that track.
func attachRTP(path string, idx *FileIndex) *media.MediaError {
	ti := idx.videoTrack()
	if ti == nil {
		return media.NewError(media.KindMissingTrack, path, "RTP sidecar but no video track")
	}
	f, err := os.Open(path)
	if err != nil {
		return media.NewError(media.KindIO, path, err.Error())
	}
	defer f.Close()

	base := media.Ticks(0)
	if len(ti.Edits) > 0 && ti.Edits[0].MediaTime > 0 {
		base = media.Ticks(uint64(ti.Edits[0].MediaTime) * uint64(media.ClockRate) /
			uint64(ti.Timescale))
	}
	unwrapper := media.NewWrapUnwrapper(base)
	corrector := media.NewDriftCorrector(0.1, 2)

	n := ti.SampleCount()
	corrected := make([]media.Ticks, n)
	stats := &RTPStats{}
	hdr := make([]byte, 2)
	packetBuf := make([]byte, 1500)
	for i := 0; i < n; i++ {
		if _, err := f.Read(hdr); err != nil {
			return media.NewError(media.KindIO, path, "truncated RTP sidecar header").
				WithTrack(ti.TrackID)
		}
		plen := int(binary.BigEndian.Uint16(hdr))
		if plen == 0 || plen > len(packetBuf) {
			return media.NewError(media.KindBadSampleTable, path, "bad RTP packet length").
				WithTrack(ti.TrackID)
		}
		if _, err := readFull(f, packetBuf[:plen]); err != nil {
			return media.NewError(media.KindIO, path, "truncated RTP packet").
				WithTrack(ti.TrackID)
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(packetBuf[:plen]); err != nil {
			return media.NewError(media.KindBadSampleTable, path, err.Error()).
				WithTrack(ti.TrackID)
		}
		res, ok := unwrapper.Push(pkt.SequenceNumber, pkt.Timestamp)
		if !ok {
			ti.Stopped = true
			return media.NewError(media.KindTimestampWrap, path,
				"backward timestamp cannot be proven as 32-bit wrap").
				WithTrack(ti.TrackID).
				WithInterval(ti.StartTick[i], ti.StartTick[i]+ti.Duration[i])
		}
		ref := base + ti.StartTick[i]
		corr, _ := corrector.Update(pkt.SequenceNumber,
			media.DriftSample{Observed: res.Unwrapped, Reference: ref})
		corrected[i] = corr
		stats.Packets++
		if res.Wrapped {
			if stats.WrapCount == 0 {
				stats.FirstWrapSeq = pkt.SequenceNumber
			}
			stats.WrapCount++
		}
	}
	stats.TotalCorrection = corrector.TotalCorrection()
	ti.Corrected = corrected
	ti.RTP = stats
	return nil
}

// VideoTrack returns the parsed video track index, or nil.
func (idx *FileIndex) VideoTrack() *TrackIndex { return idx.videoTrack() }

// TrackByID returns the track with the requested id.
func (idx *FileIndex) TrackByID(id uint32) *TrackIndex {
	for i := range idx.Tracks {
		if idx.Tracks[i].TrackID == id {
			return &idx.Tracks[i]
		}
	}
	return nil
}

func (idx *FileIndex) videoTrack() *TrackIndex {
	for i := range idx.Tracks {
		if idx.Tracks[i].Handler == "vide" {
			return &idx.Tracks[i]
		}
	}
	if len(idx.Tracks) > 0 {
		return &idx.Tracks[0]
	}
	return nil
}

func readFull(f *os.File, buf []byte) (int, error) {
	got := 0
	for got < len(buf) {
		n, err := f.Read(buf[got:])
		got += n
		if err != nil {
			return got, err
		}
	}
	return got, nil
}
