package segment

import (
	"encoding/binary"
	"fmt"

	"aircheck/internal/media"
)

// DemuxCMAF parses one CMAF media segment (styp+moof+mdat) using the supplied
// initialisation section. Sample timing comes from tfdt + trun; sample sizes
// from trun. A media segment without an init section fails FaultMissingInit.
// Box overruns fail FaultBadBoxLength; short data inside a well-formed box
// fails FaultTruncated; both name the original byte offset.
func DemuxCMAF(artifact string, data []byte, opt FMP4Options) (*Result, error) {
	if len(opt.Init) == 0 {
		return nil, media.NewFault(media.FaultMissingInit,
			media.SourcePos{Artifact: artifact, Offset: 0},
			"CMAF media segment has no initialisation section")
	}
	res := &Result{Format: FormatCMAF, Artifact: artifact, SizeBytes: int64(len(data)), InitBytes: int64(len(opt.Init))}

	tracks, trex, err := parseInit(opt.InitName, opt.Init)
	if err != nil {
		return nil, err
	}
	for _, t := range tracks {
		res.Tracks = append(res.Tracks, Track{
			Kind: t.kind, Codec: t.codec, Timescale: t.timescale,
			SampleRate: t.sampleRate, Channels: t.channels,
			Width: t.width, Height: t.height, TrackID: t.id, Init: t.init,
		})
	}

	boxes, err := ReadBoxes(artifact, data, 0, 0)
	if err != nil {
		return nil, err
	}
	var haveMoof bool
	for _, b := range boxes {
		switch b.Type {
		case boxMOOF:
			haveMoof = true
			if err := parseMoof(res, artifact, data, b, tracks, trex); err != nil {
				return nil, err
			}
		case boxMDAT:
			// mdat bytes are consumed via trun offsets; nothing to do.
		case boxFTYP, "styp", "sidx", "emsg", "prft":
		default:
			// Unknown top-level boxes are tolerated only when well-formed,
			// which ReadBoxes already guaranteed.
		}
	}
	if !haveMoof {
		return nil, media.NewFault(media.FaultMissingInit,
			media.SourcePos{Artifact: artifact, Offset: 0}, "CMAF segment has no moof")
	}
	if len(res.Packets) == 0 {
		return nil, media.NewFault(media.FaultTruncated,
			media.SourcePos{Artifact: artifact, Offset: 0}, "CMAF segment carries no samples")
	}
	res.Sort()
	return res, nil
}

// parseInit splits the init section into ftyp and moov and extracts tracks.
func parseInit(initName string, data []byte) ([]trackInfo, map[uint32]trexDefaults, error) {
	if initName == "" {
		initName = "<init>"
	}
	boxes, err := ReadBoxes(initName, data, 0, 0)
	if err != nil {
		return nil, nil, err
	}
	_, hasFtyp := find(boxes, boxFTYP)
	moov, hasMoov := find(boxes, boxMOOV)
	if !hasFtyp || !hasMoov {
		return nil, nil, media.NewFault(media.FaultMissingInit,
			media.SourcePos{Artifact: initName, Offset: 0}, "init section lacks ftyp or moov")
	}
	traks, err := parseMoov(initName, data, moov, 0)
	if err != nil {
		return nil, nil, err
	}
	if len(traks) == 0 {
		return nil, nil, media.NewFault(media.FaultMissingInit,
			media.SourcePos{Artifact: initName, Offset: moov.Offset}, "moov has no trak")
	}
	trex, err := parseTrex(initName, data, moov, 0)
	if err != nil {
		return nil, nil, err
	}
	return traks, trex, nil
}

// parseMoof walks one moof's traf boxes, collecting samples from trun runs.
func parseMoof(res *Result, artifact string, data []byte, moof Box, tracks []trackInfo, trex map[uint32]trexDefaults) error {
	kids, err := childBoxes(artifact, data, moof, 0)
	if err != nil {
		return err
	}
	var haveTraf bool
	for _, traf := range kids {
		if traf.Type != "traf" {
			continue
		}
		haveTraf = true
		if err := parseTraf(res, artifact, data, traf, moof, tracks, trex); err != nil {
			return err
		}
	}
	if !haveTraf {
		return media.NewFault(media.FaultBadBoxLength,
			media.SourcePos{Artifact: artifact, Offset: moof.Offset}, "moof has no traf")
	}
	return nil
}

func parseTraf(res *Result, artifact string, data []byte, traf, moof Box, tracks []trackInfo, trex map[uint32]trexDefaults) error {
	kids, err := childBoxes(artifact, data, traf, 0)
	if err != nil {
		return err
	}
	tfhd, ok := find(kids, boxTFHD)
	if !ok {
		return media.NewFault(media.FaultBadBoxLength,
			media.SourcePos{Artifact: artifact, Offset: traf.Offset}, "traf has no tfhd")
	}
	_, tfFlags, tfhdBody, err := fullBox(artifact, data, tfhd, 0)
	if err != nil {
		return err
	}
	if len(tfhdBody) < 8 {
		return media.NewFault(media.FaultTruncated,
			media.SourcePos{Artifact: artifact, Offset: tfhd.Offset}, "tfhd short")
	}
	trackID := binary.BigEndian.Uint32(tfhdBody[4:8])
	flags := tfFlags
	_ = flags
	var baseDataOffset int64
	var defaultDur, defaultSize int64
	pos := 8
	if flags&0x000001 != 0 { // base-data-offset
		if len(tfhdBody) < pos+8 {
			return media.NewFault(media.FaultTruncated, media.SourcePos{Artifact: artifact, Offset: tfhd.Offset}, "tfhd base-data-offset truncated")
		}
		baseDataOffset = int64(binary.BigEndian.Uint64(tfhdBody[pos : pos+8]))
		pos += 8
	} else {
		baseDataOffset = moof.Offset
	}
	_ = flags
	if flags&0x000002 != 0 { // sample-description-index
		pos += 4
	}
	if flags&0x000008 != 0 { // default-sample-duration
		if len(tfhdBody) < pos+4 {
			return media.NewFault(media.FaultTruncated, media.SourcePos{Artifact: artifact, Offset: tfhd.Offset}, "tfhd duration truncated")
		}
		defaultDur = int64(binary.BigEndian.Uint32(tfhdBody[pos : pos+4]))
		pos += 4
	}
	if flags&0x000010 != 0 { // default-sample-size
		if len(tfhdBody) < pos+4 {
			return media.NewFault(media.FaultTruncated, media.SourcePos{Artifact: artifact, Offset: tfhd.Offset}, "tfhd size truncated")
		}
		defaultSize = int64(binary.BigEndian.Uint32(tfhdBody[pos : pos+4]))
		pos += 4
	}
	if d, ok := trex[trackID]; ok {
		if defaultDur == 0 {
			defaultDur = d.defaultDur
		}
		if defaultSize == 0 {
			defaultSize = d.defaultSize
		}
	}
	ti, ok := trackByIDList(tracks, trackID)
	if !ok {
		return media.NewFault(media.FaultMissingInit,
			media.SourcePos{Artifact: artifact, Offset: traf.Offset},
			fmt.Sprintf("traf track %d absent from init section", trackID))
	}

	var decodeTime int64
	if tfdt, ok := find(kids, boxTFDT); ok {
		ver, _, body, err := fullBox(artifact, data, tfdt, 0)
		if err != nil {
			return err
		}
		if ver == 1 {
			if len(body) < 12 {
				return media.NewFault(media.FaultTruncated, media.SourcePos{Artifact: artifact, Offset: tfdt.Offset}, "tfdt v1 short")
			}
			decodeTime = int64(binary.BigEndian.Uint64(body[4:12]))
		} else {
			if len(body) < 8 {
				return media.NewFault(media.FaultTruncated, media.SourcePos{Artifact: artifact, Offset: tfdt.Offset}, "tfdt v0 short")
			}
			decodeTime = int64(binary.BigEndian.Uint32(body[4:8]))
		}
	} else {
		return media.NewFault(media.FaultMissingInit,
			media.SourcePos{Artifact: artifact, Offset: traf.Offset}, "traf has no tfdt base decode time")
	}

	runDataOffset := baseDataOffset
	var runCursor int64 = -1
	for _, k := range kids {
		if k.Type != boxTRUN {
			continue
		}
		samples, next, newCursor, err := parseTrun(artifact, data, k, runDataOffset, runCursor, defaultDur, defaultSize)
		if err != nil {
			return err
		}
		runDataOffset = next
		runCursor = newCursor
		for _, s := range samples {
			res.Packets = append(res.Packets, Packet{
				Kind: ti.kind, TrackID: ti.id,
				DTS: decodeTime, PTS: decodeTime,
				Duration: s.dur, Samples: s.audioSamples(ti),
				Keyframe: s.key, Data: s.data, SourceOffset: s.offset,
			})
			// Video sample timestamps advance by duration; PTS composition
			// offsets are not modelled (fixtures use zero), so PTS == DTS.
			decodeTime += s.dur
		}
	}
	return nil
}

// parseTrun decodes one trun's samples and their data extents. Sample data
// offsets are relative to baseDataOffset (tfhd base-data-offset or moof start);
// a trun without its own data offset continues the previous run's cursor.
func parseTrun(artifact string, data []byte, trun Box, baseDataOffset, cursor, defaultDur, defaultSize int64) ([]sampleRec, int64, int64, error) {
	ver, flags, body, err := fullBox(artifact, data, trun, 0)
	if err != nil {
		return nil, 0, 0, err
	}
	_ = ver
	if len(body) < 8 {
		return nil, 0, 0, media.NewFault(media.FaultTruncated,
			media.SourcePos{Artifact: artifact, Offset: trun.Offset}, "trun short")
	}
	count := int(binary.BigEndian.Uint32(body[4:8]))
	pos := 8
	if count > 1<<20 {
		return nil, 0, 0, media.NewFault(media.FaultBadBoxLength,
			media.SourcePos{Artifact: artifact, Offset: trun.Offset},
			fmt.Sprintf("trun sample count %d implausible", count))
	}
	var dataOffset int64
	haveDataOffset := flags&0x000001 != 0
	if haveDataOffset {
		if len(body) < pos+4 {
			return nil, 0, 0, media.NewFault(media.FaultTruncated,
				media.SourcePos{Artifact: artifact, Offset: trun.Offset}, "trun data-offset truncated")
		}
		dataOffset = int64(int32(binary.BigEndian.Uint32(body[pos : pos+4])))
		pos += 4
	}
	var firstFlags uint32
	if flags&0x000004 != 0 { // first-sample-flags
		if len(body) < pos+4 {
			return nil, 0, 0, media.NewFault(media.FaultTruncated,
				media.SourcePos{Artifact: artifact, Offset: trun.Offset}, "trun first-sample-flags truncated")
		}
		firstFlags = binary.BigEndian.Uint32(body[pos : pos+4])
		pos += 4
	}

	cursorV := cursor
	if haveDataOffset {
		cursorV = baseDataOffset + dataOffset
	} else if cursorV < 0 {
		return nil, 0, 0, media.NewFault(media.FaultMissingInit,
			media.SourcePos{Artifact: artifact, Offset: trun.Offset},
			"trun lacks a data offset and no run is open")
	}

	perSample := 0
	for _, bit := range []uint32{0x000100, 0x000200, 0x000400, 0x000800} {
		if flags&bit != 0 {
			perSample += 4
		}
	}
	if perSample > 0 && count > 0 && len(body) < pos+count*perSample {
		return nil, 0, 0, media.NewFault(media.FaultTruncated,
			media.SourcePos{Artifact: artifact, Offset: trun.Offset},
			fmt.Sprintf("trun declares %d samples but its body holds %d bytes", count, len(body)-pos))
	}

	off := int64(0)
	out := make([]sampleRec, 0, count)
	for i := 0; i < count; i++ {
		dur, size := defaultDur, defaultSize
		var sflags uint32
		if i == 0 && flags&0x000004 != 0 {
			sflags = firstFlags
		}
		if flags&0x000100 != 0 { // sample-duration
			dur = int64(binary.BigEndian.Uint32(body[pos : pos+4]))
			pos += 4
		}
		if flags&0x000200 != 0 { // sample-size
			size = int64(binary.BigEndian.Uint32(body[pos : pos+4]))
			pos += 4
		}
		if flags&0x000400 != 0 { // sample-flags (per sample)
			sflags = binary.BigEndian.Uint32(body[pos : pos+4])
			pos += 4
		}
		if flags&0x000800 != 0 { // composition offset; fixtures keep PTS==DTS
			pos += 4
		}
		if dur <= 0 {
			return nil, 0, 0, media.NewFault(media.FaultBadBoxLength,
				media.SourcePos{Artifact: artifact, Offset: trun.Offset},
				fmt.Sprintf("sample %d has no duration", i))
		}
		key := true
		if flags&0x000004 != 0 || flags&0x000400 != 0 {
			key = sflags&0x00010000 == 0 // absence of sample_is_non_sync
		}
		if size < 0 {
			return nil, 0, 0, media.NewFault(media.FaultBadBoxLength,
				media.SourcePos{Artifact: artifact, Offset: trun.Offset},
				fmt.Sprintf("sample %d size %d negative", i, size))
		}
		start := cursorV + off
		end := start + size
		if start < 0 || end > int64(len(data)) {
			return nil, 0, 0, media.NewFault(media.FaultTruncated,
				media.SourcePos{Artifact: artifact, Offset: start},
				fmt.Sprintf("sample %d data [%d,%d) outside segment of %d bytes", i, start, end, len(data)))
		}
		out = append(out, sampleRec{dur: dur, data: data[start:end], offset: start, key: key})
		off += size
	}
	return out, cursorV, cursorV + off, nil
}

type sampleRec struct {
	dur    int64
	data   []byte
	offset int64
	key    bool
}

// audioSamples converts a duration in the track timescale to a sample count.
func (s sampleRec) audioSamples(ti trackInfo) int {
	if ti.kind != media.KindAudio || ti.sampleRate <= 0 {
		return 0
	}
	return int(s.dur * int64(ti.sampleRate) / ti.timescale)
}

func trackByIDList(list []trackInfo, id uint32) (trackInfo, bool) {
	for _, t := range list {
		if t.id == id {
			return t, true
		}
	}
	return trackInfo{}, false
}
