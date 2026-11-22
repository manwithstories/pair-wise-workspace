package ingest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"mediarun/internal/mediaerr"
	"mediarun/internal/timebase"
)

// parsePartition reads the single leading partition pack and its tagged
// descriptor items, building one mxfTrack per declared essence.
func (s *mxfStreamer) parsePartition() error {
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(s.path)
	}
	s.pos = 0
	ul, err := s.readUL("partition pack")
	if err != nil {
		return err
	}
	if ul != mxfPartitionUL {
		return mediaerr.New(mediaerr.CodeUnknownStructure, fmt.Sprintf(
			"unrecognized partition UL %x", ul)).At("mxf", "", 0).OnPath(s.path)
	}
	packLen, err := s.readBER("partition payload")
	if err != nil {
		return err
	}
	if s.pos+packLen > s.size {
		return mediaerr.New(mediaerr.CodeTruncatedKLV, fmt.Sprintf(
			"partition payload %d bytes runs past EOF %d", packLen, s.size)).
			At("mxf", "", s.pos).OnPath(s.path)
	}
	itemCount, err := s.readBER("partition item count")
	if err != nil {
		return err
	}
	for i := int64(0); i < itemCount; i++ {
		if err := s.readDescriptor(); err != nil {
			return err
		}
	}
	if len(s.tracks) == 0 {
		return mediaerr.New(mediaerr.CodeUnknownStructure,
			"partition declares no essence tracks").At("mxf", "", 0).OnPath(s.path)
	}
	for _, id := range s.order {
		s.info.Tracks = append(s.info.Tracks, s.tracks[id].desc)
		s.totalN += s.tracks[id].desc.SampleCount
	}
	return nil
}

// readDescriptor consumes one tagged descriptor item (tag + BER len + body).
func (s *mxfStreamer) readDescriptor() error {
	tagBuf := make([]byte, 2)
	if _, err := io.ReadFull(s.file, tagBuf); err != nil {
		return mediaerr.New(mediaerr.CodeTruncatedKLV, "truncated descriptor tag").
			At("mxf", "", s.pos).OnPath(s.path)
	}
	s.pos += 2
	tag := binary.BigEndian.Uint16(tagBuf)
	bodyLen, err := s.readBER("descriptor body")
	if err != nil {
		return err
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(s.file, body); err != nil {
		return mediaerr.New(mediaerr.CodeTruncatedKLV, "truncated descriptor body").
			At("mxf", "", s.pos).OnPath(s.path)
	}
	s.pos += bodyLen
	return s.applyDescriptor(tag, body)
}

func (s *mxfStreamer) applyDescriptor(tag uint16, body []byte) error {
	kind := TrackKind(string(body))
	if tag == 2 { // kind opens a new track descriptor
		id := fmt.Sprintf("%s/%d", kind, len(s.order))
		tr := &mxfTrack{desc: TrackDesc{ID: id, Kind: kind}}
		s.tracks[id] = tr
		s.order = append(s.order, id)
		return nil
	}
	if len(s.order) == 0 {
		return mediaerr.New(mediaerr.CodeUnknownStructure,
			"descriptor item before any kind tag").At("mxf", "", s.pos).OnPath(s.path)
	}
	tr := s.tracks[s.order[len(s.order)-1]]
	switch tag {
	case 3:
		tr.desc.Codec = string(body)
	case 4:
		tr.desc.Timescale = decodeBERBytes(body)
		tr.tb = timebase.NewTimebase(tr.desc.Timescale)
	case 5:
		tr.desc.Width = uint32(decodeBERBytes(body))
	case 6:
		tr.desc.Height = uint32(decodeBERBytes(body))
	case 7:
		tr.desc.Channels = uint16(decodeBERBytes(body))
	case 8:
		tr.desc.SampleCount = decodeBERBytes(body)
	default:
		return mediaerr.New(mediaerr.CodeUnknownStructure, fmt.Sprintf(
			"unknown descriptor tag %d", tag)).At("mxf", "", s.pos).OnPath(s.path)
	}
	return nil
}

// decodeBERBytes interprets a body that stores a BER-encoded integer value.
func decodeBERBytes(b []byte) int64 {
	if len(b) == 0 {
		return 0
	}
	if b[0] < 0x80 {
		return int64(b[0])
	}
	n := int(b[0] & 0x7f)
	if n == 0 || n > len(b)-1 {
		return 0
	}
	var v int64
	for i := 1; i <= n; i++ {
		v = (v << 8) | int64(b[i])
	}
	return v
}

// Next streams essence samples in physical (interleaved) file order, which
// for the generated OP1a subset is the same order the writer used.
func (s *mxfStreamer) Next() (*SampleEvent, error) {
	if s.picked >= s.totalN {
		return nil, io.EOF
	}
	ul, err := s.readUL("essence sample")
	if err != nil {
		return nil, err
	}
	var trackID string
	switch {
	case bytes.Equal(ul[:], mxfVideoEssenceUL[:]):
		trackID = s.trackIDOfKind(KindVideo)
	case bytes.Equal(ul[:], mxfAudioEssenceUL[:]):
		trackID = s.trackIDOfKind(KindAudio)
	default:
		return nil, mediaerr.New(mediaerr.CodeUnknownStructure, fmt.Sprintf(
			"essence UL %x matches no declared track", ul)).
			At("mxf", "", s.pos-16).OnPath(s.path)
	}
	tr := s.tracks[trackID]
	length, err := s.readBER("essence length")
	if err != nil {
		return nil, err
	}
	if length > MaxBlockBytes {
		return nil, mediaerr.New(mediaerr.CodeBlockTooLarge, fmt.Sprintf(
			"essence sample size %d exceeds 4 MiB ceiling", length)).
			At("mxf", trackID, s.pos).OnPath(s.path)
	}
	dataOffset := s.pos
	if dataOffset+length > s.size {
		return nil, mediaerr.New(mediaerr.CodeTruncatedKLV, fmt.Sprintf(
			"essence payload [%d,%d) runs past EOF %d", dataOffset, dataOffset+length, s.size)).
			At("mxf", trackID, dataOffset).OnPath(s.path)
	}
	if tr.hasDTS {
		// Samples are uniformly one edit unit; detect any regression defensively.
		if tr.next < 0 {
			return nil, mediaerr.New(mediaerr.CodeDTSRegression,
				"negative sample index after wraparound").
				At("mxf", trackID, dataOffset).OnPath(s.path)
		}
	}
	tr.hasDTS = true
	ev := &SampleEvent{
		TrackID:    trackID,
		Index:      tr.next,
		DataOffset: dataOffset,
		Size:       length,
		DTS:        tr.next,
		PTS:        tr.next,
		Duration:   1,
		Timebase:   tr.tb,
		Payload:    io.NewSectionReader(s.file, dataOffset, length),
	}
	tr.next++
	s.pos += length
	s.picked++
	return ev, nil
}

func (s *mxfStreamer) trackIDOfKind(k TrackKind) string {
	for _, id := range s.order {
		if s.tracks[id].desc.Kind == k {
			return id
		}
	}
	return ""
}
