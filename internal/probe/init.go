package probe

import (
	"bytes"
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"mediad/internal/fault"
)

// TrackInfo is the timing context declared by an initialization segment.
type TrackInfo struct {
	TrackID   uint32
	Timescale uint32
	Duration  uint64
	HasMoov   bool
	Handler   string
}

// InitInfo summarises the ftyp/moov structure of an init segment.
type InitInfo struct {
	MajorBrand string
	Tracks     []TrackInfo
	HasFtyp    bool
	HasMoov    bool
}

// ParseInit reads ftyp and moov/mvhd/trak/mdia/mdhd from an init segment.
// Structural faults are returned alongside any tracks that did parse.
func ParseInit(r io.ReadSeeker, size int64, uri string) (*InitInfo, *fault.List, error) {
	var faults fault.List
	boxes, sf := scanBoxes(r, 0, size, maxBoxes)
	faults.Add(sf...)
	info := &InitInfo{}
	for _, b := range boxes {
		switch b.Type {
		case "ftyp":
			info.HasFtyp = true
			if payload, err := readPayload(r, b); err == nil {
				var ftyp mp4.Ftyp
				if _, err := mp4.Unmarshal(bytes.NewReader(payload), uint64(len(payload)), &ftyp, mp4.Context{}); err == nil {
					info.MajorBrand = string(ftyp.MajorBrand[:])
				}
			}
		case "moov":
			info.HasMoov = true
			tracks, tf := parseMoov(r, b, uri)
			faults.Add(tf...)
			info.Tracks = append(info.Tracks, tracks...)
		}
	}
	if !info.HasMoov {
		faults.Add(fault.MustNew(fault.KindMissingMoof,
			fault.Location{Segment: uri, Offset: 0}, "initialization segment has no moov", false, false))
	}
	return info, &faults, nil
}

func parseMoov(r io.ReadSeeker, moov Box, uri string) ([]TrackInfo, []fault.Fault) {
	var faults []fault.Fault
	var tracks []TrackInfo
	children, cf := scanBoxes(r, moov.PayloadOffset(), moov.End(), 4096)
	faults = append(faults, cf...)
	for _, trak := range children {
		if trak.Type != "trak" {
			continue
		}
		tr, tf := parseTrak(r, trak, uri)
		faults = append(faults, tf...)
		if tr != nil {
			tracks = append(tracks, *tr)
		}
	}
	return tracks, faults
}

func parseTrak(r io.ReadSeeker, trak Box, uri string) (*TrackInfo, []fault.Fault) {
	tr := &TrackInfo{}
	var faults []fault.Fault
	children, cf := scanBoxes(r, trak.PayloadOffset(), trak.End(), 4096)
	faults = append(faults, cf...)
	for _, child := range children {
		switch child.Type {
		case "tkhd":
			if payload, err := readPayload(r, child); err == nil {
				var tkhd mp4.Tkhd
				if _, err := mp4.Unmarshal(bytes.NewReader(payload), uint64(len(payload)), &tkhd, mp4.Context{}); err == nil {
					tr.TrackID = tkhd.TrackID
				}
			}
		case "mdia":
			mdiaChildren, mf := scanBoxes(r, child.PayloadOffset(), child.End(), 4096)
			faults = append(faults, mf...)
			for _, mc := range mdiaChildren {
				switch mc.Type {
				case "mdhd":
					if payload, err := readPayload(r, mc); err == nil {
						var mdhd mp4.Mdhd
						if _, err := mp4.Unmarshal(bytes.NewReader(payload), uint64(len(payload)), &mdhd, mp4.Context{}); err == nil {
							tr.Timescale = mdhd.Timescale
							tr.Duration = mdhd.GetDuration()
						}
					}
				case "hdlr":
					if payload, err := readPayload(r, mc); err == nil {
						var hdlr mp4.Hdlr
						if _, err := mp4.Unmarshal(bytes.NewReader(payload), uint64(len(payload)), &hdlr, mp4.Context{}); err == nil {
							tr.Handler = string(hdlr.HandlerType[:])
						}
					}
				}
			}
		}
	}
	if tr.Timescale == 0 {
		faults = append(faults, fault.MustNew(fault.KindBoxHeader,
			fault.Location{Segment: uri, Offset: trak.Offset}, "track has no valid mdhd timescale", true, false))
		return nil, faults
	}
	return tr, faults
}

func readPayload(r io.ReadSeeker, b Box) ([]byte, error) {
	if b.Size < b.HeaderSize || b.Size > maxScanOffset {
		return nil, fmt.Errorf("probe: invalid payload size %d", b.Size)
	}
	payload := make([]byte, b.Size-b.HeaderSize)
	if _, err := r.Seek(b.PayloadOffset(), io.SeekStart); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}
