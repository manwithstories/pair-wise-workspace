// Package mux remuxes parsed access units into a standalone MP4 container
// with github.com/abema/go-mp4. Encoded payloads are copied byte-for-byte:
// the muxer only changes framing (Annex B start codes -> length-prefixed
// samples) and never decodes or re-encodes media.
package mux

import (
	"encoding/binary"

	"github.com/abema/go-mp4"

	"mediapack/internal/codec"
)

// Sample is one length-prefixed MP4 sample assembled from an access unit.
type Sample struct {
	Data    []byte
	Size    uint32
	Key     bool
	PTS     int64
	Seq     int64
	NALCount int
}

// buildSamples converts access units to MP4 samples with 4-byte NAL lengths.
// The synthetic integrity trailer (codec-internal framing) is stripped so the
// container holds only codec-payload units.
func buildSamples(aus []codec.AccessUnit) []Sample {
	out := make([]Sample, 0, len(aus))
	for _, au := range aus {
		var data []byte
		for _, n := range au.NALs {
			body := stripTrailer(n.Raw)
			var lenBuf [4]byte
			binary.BigEndian.PutUint32(lenBuf[:], uint32(len(body)))
			data = append(data, lenBuf[:]...)
			data = append(data, body...)
		}
		out = append(out, Sample{
			Data: data, Size: uint32(len(data)), Key: au.Key,
			PTS: au.PTS, Seq: au.Seq, NALCount: len(au.NALs),
		})
	}
	return out
}

func stripTrailer(raw []byte) []byte {
	if len(raw) >= 8 && raw[len(raw)-8] == 'M' && raw[len(raw)-7] == 'P' &&
		raw[len(raw)-6] == 'K' && raw[len(raw)-5] == '1' {
		return raw[:len(raw)-8]
	}
	return raw
}

// avcConfig builds the avcC record from parsed SPS/PPS parameter sets.
func avcConfig(meta *codec.StreamMeta) *mp4.AVCDecoderConfiguration {
	cfg := &mp4.AVCDecoderConfiguration{
		ConfigurationVersion: 1,
		LengthSizeMinusOne:   3, // 4-byte length prefix
	}
	cfg.SetType(mp4.BoxTypeAvcC())
	for _, n := range meta.Configs {
		body := stripTrailer(n)
		t := body[0] & 0x1F
		ps := mp4.AVCParameterSet{NALUnit: body}
		switch t {
		case 7: // SPS
			cfg.Profile = body[1]
			cfg.ProfileCompatibility = body[2]
			cfg.Level = body[3]
			cfg.SequenceParameterSets = append(cfg.SequenceParameterSets, ps)
		case 8: // PPS
			cfg.PictureParameterSets = append(cfg.PictureParameterSets, ps)
		}
	}
	return cfg
}

// hevcConfig builds the hvcC record grouped by NAL array type.
func hevcConfig(meta *codec.StreamMeta) *mp4.HvcC {
	cfg := &mp4.HvcC{
		ConfigurationVersion: 1,
		LengthSizeMinusOne:   3,
		GeneralProfileIdc:    1,
		GeneralLevelIdc:      30,
	}
	arrays := map[uint8]*mp4.HEVCNaluArray{}
	for _, n := range meta.Configs {
		body := stripTrailer(n)
		t := (body[0] >> 1) & 0x3F
		arr, ok := arrays[t]
		if !ok {
			arr = &mp4.HEVCNaluArray{NaluType: t, Completeness: true}
			arrays[t] = arr
		}
		arr.Nalus = append(arr.Nalus, mp4.HEVCNalu{NALUnit: body})
		arr.NumNalus = uint16(len(arr.Nalus))
	}
	for _, arr := range arrays {
		cfg.NaluArrays = append(cfg.NaluArrays, *arr)
	}
	cfg.NumOfNaluArrays = uint8(len(cfg.NaluArrays))
	return cfg
}

// av1Config builds the minimal av1C record.
func av1Config() *mp4.Av1C {
	return &mp4.Av1C{
		Marker: 1, Version: 1, SeqProfile: 0, SeqLevelIdx0: 8,
		ChromaSubsamplingX: 1, ChromaSubsamplingY: 1,
	}
}
