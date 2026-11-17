package hdr

import (
	"errors"
	"fmt"
	"sort"

	"hdrcheck/internal/garc"
)

// SEI payload 类型。
const (
	SEIPayloadMastering uint8 = 1
	SEIPayloadCLL       uint8 = 2
)

// SEIRecord 是一条成功解析的视频轨 HDR-SEI 记录。
type SEIRecord struct {
	TrackID   uint32             `json:"track_id"`
	SeqNum    uint32             `json:"seq_num"`
	TimeStart uint32             `json:"time_start_ms"`
	TimeEnd   uint32             `json:"time_end_ms"`
	Mastering *MasteringDisplay  `json:"mastering_display,omitempty"`
	CLL       *ContentLightLevel `json:"cll,omitempty"`
	Offset    int64              `json:"offset"`
}

// SEIResult 是整个 SEI 区的解析结果，按轨分组并保留全部问题。
type SEIResult struct {
	Records []SEIRecord `json:"records"`
	Issues  []Issue     `json:"issues"`
	// Fatal 表示 SEI 区结构损坏到无法继续（截断/越界），退出码按 3 处理。
	Fatal bool `json:"-"`
}

// RecordsOf 返回指定轨的记录（按文件中的出现顺序）。
func (s *SEIResult) RecordsOf(trackID uint32) []SEIRecord {
	var out []SEIRecord
	for _, rec := range s.Records {
		if rec.TrackID == trackID {
			out = append(out, rec)
		}
	}
	return out
}

// seiFixedHead 是 payload 之前的固定头：4+4+4+4+1 = 17 字节。
const seiFixedHead = 17

// ParseSEI 解析 SEI 区。区长度为 0 时返回空结果。校验失败的记录被丢弃
// （不参与后续合并），但 CHECKSUM_ERROR 仍记录在案。
func ParseSEI(f *garc.File) (*SEIResult, error) {
	out := &SEIResult{}
	rg := f.Header.SEI
	if rg.Length == 0 {
		return out, nil
	}
	r := f.Reader
	regionEnd, ok := rg.End()
	if !ok {
		return nil, garc.NewError("INTEGER_OVERFLOW", int64(rg.Offset),
			"sei region offset+length overflows uint32")
	}
	cursor := int64(rg.Offset)
	end := int64(regionEnd)
	knownTracks := map[uint32]bool{}
	for _, t := range f.Tracks {
		knownTracks[t.ID] = true
	}

	for cursor < end {
		if end-cursor < seiFixedHead {
			out.Fatal = true
			out.Issues = append(out.Issues, truncIssue(cursor, "SEI record header"))
			return out, nil
		}
		recStart := cursor
		trackID, _ := r.U32(cursor)
		seqNum, _ := r.U32(cursor + 4)
		tStart, _ := r.U32(cursor + 8)
		tEnd, _ := r.U32(cursor + 12)
		payloadType, _ := r.U8(cursor + 16)
		pLen, vn, err := r.Varint(cursor + int64(seiFixedHead))
		if err != nil {
			var ge *garc.Error
			if errors.As(err, &ge) {
				out.Fatal = ge.Code == "TRUNCATED_RECORD"
				out.Issues = append(out.Issues, Issue{
					Code: ge.Code, Severity: SeverityError, Offset: ge.Offset,
					Message: ge.Message, Hint: ge.Hint,
				})
			}
			return out, nil
		}
		payloadOff := cursor + seiFixedHead + int64(vn)
		// 记录总长 = 固定头 + varint + payload + crc32。
		recLen := int64(seiFixedHead) + int64(vn) + int64(pLen) + 4
		if recLen > end-cursor {
			out.Fatal = true
			out.Issues = append(out.Issues, truncIssue(recStart,
				fmt.Sprintf("SEI record track=%d seq=%d payload", trackID, seqNum)))
			return out, nil
		}
		crcOff := payloadOff + int64(pLen)
		wantCRC, _ := r.U32(crcOff)
		if err := r.CheckCRC(recStart, crcOff-recStart, wantCRC); err != nil {
			var ge *garc.Error
			if errors.As(err, &ge) {
				ge.TrackID = trackID
				out.Issues = append(out.Issues, Issue{
					Code: ge.Code, Severity: SeverityError, Offset: ge.Offset,
					TrackID: trackID, Message: ge.Message, Hint: ge.Hint,
				})
			}
			cursor = recStart + recLen
			continue
		}
		if tEnd <= tStart {
			out.Issues = append(out.Issues, Issue{
				Code: "ILLEGAL_TIME_RANGE", Severity: SeverityError,
				TrackID: trackID, Offset: recStart,
				Message: fmt.Sprintf("time_end=%d must be > time_start=%d", tEnd, tStart),
				Hint:    "re-export with a positive-duration SEI record",
			})
			cursor = recStart + recLen
			continue
		}
		if !knownTracks[trackID] {
			out.Issues = append(out.Issues, Issue{
				Code: "ORPHAN_SEI_RECORD", Severity: SeverityError,
				TrackID: trackID, Offset: recStart,
				Message: fmt.Sprintf("SEI record references unknown track_id %d", trackID),
				Hint:    "re-export; SEI must attach to a track in the track table",
			})
			cursor = recStart + recLen
			continue
		}
		rec := SEIRecord{
			TrackID: trackID, SeqNum: seqNum,
			TimeStart: tStart, TimeEnd: tEnd, Offset: recStart,
		}
		switch payloadType {
		case SEIPayloadMastering:
			if pLen != masteringPayloadLen {
				out.badSEIDimension(trackID, recStart, pLen, masteringPayloadLen)
				cursor = recStart + recLen
				continue
			}
			rec.Mastering = decodeSEIMastering(r, payloadOff, trackID, seqNum, tStart, tEnd)
		case SEIPayloadCLL:
			if pLen != cllPayloadLen {
				out.badSEIDimension(trackID, recStart, pLen, cllPayloadLen)
				cursor = recStart + recLen
				continue
			}
			rec.CLL = decodeSEICLL(r, payloadOff, trackID, seqNum, tStart, tEnd)
		default:
			out.Issues = append(out.Issues, Issue{
				Code: "UNKNOWN_FIELD", Severity: SeverityWarning,
				TrackID: trackID, Offset: recStart,
				Message: fmt.Sprintf("unknown SEI payload_type=%d (%d bytes) skipped", payloadType, pLen),
				Hint:    "upgrade hdrcheck if this payload type is expected",
			})
			cursor = recStart + recLen
			continue
		}
		out.Records = append(out.Records, rec)
		cursor = recStart + recLen
	}
	detectOverlaps(out)
	return out, nil
}

func decodeSEIMastering(r *garc.Reader, off int64, trackID, seq, tStart, tEnd uint32) *MasteringDisplay {
	prov := Provenance{
		Offset: off, TrackID: trackID, SeqNum: seq,
		TimeStart: tStart, TimeEnd: tEnd,
	}
	md := &MasteringDisplay{Present: true, Source: SourceSEI, Provenance: prov}
	for i := 0; i < 6; i++ {
		md.DisplayPrimaries[i][0], _ = r.U16(off + int64(i*4))
		md.DisplayPrimaries[i][1], _ = r.U16(off + int64(i*4+2))
	}
	md.WhitePoint[0], _ = r.U16(off + 24)
	md.WhitePoint[1], _ = r.U16(off + 26)
	md.MinDisplayMasteringLuminance, _ = r.U32(off + 28)
	md.MaxDisplayMasteringLuminance, _ = r.U32(off + 32)
	return md
}

func decodeSEICLL(r *garc.Reader, off int64, trackID, seq, tStart, tEnd uint32) *ContentLightLevel {
	cll, _ := r.U16(off)
	fall, _ := r.U16(off + 2)
	prov := Provenance{
		Offset: off, TrackID: trackID, SeqNum: seq,
		TimeStart: tStart, TimeEnd: tEnd,
	}
	return &ContentLightLevel{
		MaxContentLightLevel: LightLevel{
			Present: true, Nits: cll, Source: SourceSEI, Provenance: prov,
		},
		MaxFrameAverageLightLevel: LightLevel{
			Present: true, Nits: fall, Source: SourceSEI,
			Provenance: func() Provenance { p := prov; p.Offset = off + 2; return p }(),
		},
	}
}

// detectOverlaps 按轨检查记录时间区间相交（半开区间，首尾相接不算重叠）。
func detectOverlaps(out *SEIResult) {
	byTrack := map[uint32][]SEIRecord{}
	for _, rec := range out.Records {
		byTrack[rec.TrackID] = append(byTrack[rec.TrackID], rec)
	}
	trackIDs := make([]uint32, 0, len(byTrack))
	for id := range byTrack {
		trackIDs = append(trackIDs, id)
	}
	sort.Slice(trackIDs, func(i, j int) bool { return trackIDs[i] < trackIDs[j] })
	for _, id := range trackIDs {
		recs := byTrack[id]
		sort.Slice(recs, func(i, j int) bool {
			if recs[i].TimeStart != recs[j].TimeStart {
				return recs[i].TimeStart < recs[j].TimeStart
			}
			return recs[i].SeqNum < recs[j].SeqNum
		})
		for i := 1; i < len(recs); i++ {
			if recs[i].TimeStart < recs[i-1].TimeEnd {
				out.Issues = append(out.Issues, Issue{
					Code: "RECORDS_OVERLAP", Severity: SeverityError,
					TrackID: id, Offset: recs[i].Offset,
					Message: fmt.Sprintf(
						"seq %d [%d,%d) overlaps seq %d [%d,%d)",
						recs[i].SeqNum, recs[i].TimeStart, recs[i].TimeEnd,
						recs[i-1].SeqNum, recs[i-1].TimeStart, recs[i-1].TimeEnd),
					Hint: "re-export; SEI time ranges on one track must not intersect",
				})
			}
		}
	}
}

func (s *SEIResult) badSEIDimension(trackID uint32, off int64, got, want uint64) {
	s.Issues = append(s.Issues, Issue{
		Code: "ILLEGAL_DIMENSION", Severity: SeverityError,
		TrackID: trackID, Offset: off,
		Message: fmt.Sprintf("SEI payload length=%d, want %d", got, want),
		Hint:    "re-export with the specified fixed payload layout",
	})
}
