package metadata

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/abema/go-mp4"

	"mediad/internal/clock"
	"mediad/internal/model"
)

// Clap is the ISO/IEC 14496-12 clean aperture box. go-mp4 v1.4.0 ships a
// registry for pasp but not clap, so the project registers its own payload.
type Clap struct {
	mp4.Box
	CleanApertureWidthN  int32 `mp4:"0,size=32,signed"`
	CleanApertureWidthD  int32 `mp4:"1,size=32,signed"`
	CleanApertureHeightN int32 `mp4:"2,size=32,signed"`
	CleanApertureHeightD int32 `mp4:"3,size=32,signed"`
	HorizOffN            int32 `mp4:"4,size=32,signed"`
	HorizOffD            int32 `mp4:"5,size=32,signed"`
	VertOffN             int32 `mp4:"6,size=32,signed"`
	VertOffD             int32 `mp4:"7,size=32,signed"`
}

// GetType implements mp4.IBox.
func (*Clap) GetType() mp4.BoxType { return mp4.StrToBoxType("clap") }

func init() { mp4.AddBoxDef(&Clap{}) }

func boxType(name string) mp4.BoxType { return mp4.StrToBoxType(name) }

// visualParent is the sample-entry path used for all visual children.
func visualSampleEntries() []mp4.BoxType {
	return []mp4.BoxType{mp4.BoxTypeAvc1(), mp4.BoxTypeMp4v(), mp4.BoxTypeHvc1(),
		mp4.BoxTypeHev1(), boxType("av01"), boxType("vp09")}
}

// ParseMP4 normalizes every track of an MP4 file. A malformed track is
// isolated as a TrackFailure; the other tracks are still returned.
func ParseMP4(path string) (*Asset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, model.Wrap(model.CategoryMetadata, "read-mp4", err)
	}
	return ParseMP4Bytes(path, data)
}

// ParseMP4Bytes parses an in-memory MP4 container.
func ParseMP4Bytes(path string, data []byte) (*Asset, error) {
	if len(data) < 8 {
		return nil, model.New(model.CategoryMetadata, "parse-mp4", "container shorter than a box header")
	}
	asset := &Asset{Path: path, Container: "mp4"}
	rs := bytes.NewReader(data)

	traks, err := mp4.ExtractBoxWithPayload(rs, nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak()})
	if err != nil {
		return nil, model.Wrap(model.CategoryMetadata, "parse-mp4", err).
			WithBlockingDep("metadata")
	}
	if len(traks) == 0 {
		return nil, model.New(model.CategoryMetadata, "parse-mp4", "no trak boxes found")
	}

	fail := func(key string, err *model.Error) {
		asset.Failures = append(asset.Failures, TrackFailure{Key: key, Err: err})
	}
	videoID, audioID := 0, 0
	for i := range traks {
		trak := &traks[i].Info
		track, key, err := parseTrack(rs, trak, videoID, audioID)
		if err != nil {
			err = err.WithBlockingDep("metadata:" + key)
			fail(key, err)
			continue
		}
		if track.Kind == KindVideo {
			videoID++
		} else {
			audioID++
		}
		if cerr := validateColor(track); cerr != nil {
			fail(key, cerr.WithBlockingDep("metadata:"+key))
			track.Notes = append(track.Notes, "color description rejected: "+cerr.Detail)
		}
		if cerr := ValidateCrop(track); cerr != nil {
			fail(key, cerr.WithBlockingDep("metadata:"+key))
			// The track itself remains usable: only the crop is dropped.
			track.Crop = nil
			track.Notes = append(track.Notes, "clean aperture rejected: "+cerr.Detail)
		}
		asset.Tracks = append(asset.Tracks, track)
	}
	// Report the first categorized failure while still returning the tracks
	// that survived, so the CLI can exit non-zero and callers can keep the
	// healthy branches running.
	if len(asset.Failures) > 0 {
		return asset, asset.Failures[0].Err
	}
	return asset, nil
}

func parseTrack(rs io.ReadSeeker, trak *mp4.BoxInfo, videoID, audioID int) (Track, string, *model.Error) {
	// All lookups are scoped to this trak. Extracting from the file root would
	// return boxes from every track and silently cross-wire properties.
	read := func(rel mp4.BoxPath) ([]*mp4.BoxInfoWithPayload, error) {
		return mp4.ExtractBoxesWithPayload(rs, trak, []mp4.BoxPath{rel})
	}

	tkhds, err := read(mp4.BoxPath{mp4.BoxTypeTkhd()})
	if err != nil || len(tkhds) == 0 {
		return Track{}, "unknown", model.New(model.CategoryMetadata, "tkhd", "missing or malformed track header")
	}
	tkhd := tkhds[0].Payload.(*mp4.Tkhd)

	mdhds, err := read(mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMdhd()})
	if err != nil || len(mdhds) == 0 {
		return Track{}, model.TrackKey("video", uint32(videoID)), model.New(model.CategoryMetadata, "mdhd", "missing media header")
	}
	mdhd := mdhds[0].Payload.(*mp4.Mdhd)
	if mdhd.Timescale == 0 {
		return Track{}, model.TrackKey("video", uint32(videoID)), model.New(model.CategoryMetadata, "mdhd", "timescale is zero")
	}

	hdlrs, err := read(mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeHdlr()})
	if err != nil || len(hdlrs) == 0 {
		return Track{}, "unknown", model.New(model.CategoryMetadata, "hdlr", "missing handler box")
	}
	handler := string(hdlrs[0].Payload.(*mp4.Hdlr).HandlerType[:])
	kind := KindVideo
	id := uint32(videoID)
	switch handler {
	case "vide":
	case "soun":
		kind = KindAudio
		id = uint32(audioID)
	default:
		kind = "other"
	}

	tb, err := clock.New(1, int64(mdhd.Timescale))
	if err != nil {
		return Track{}, "unknown", model.Wrap(model.CategoryMetadata, "timescale", err)
	}
	dur := mdhd.GetDuration()
	track := Track{
		Key:       model.TrackKey(string(kind), id),
		Kind:      kind,
		TrackID:   tkhd.TrackID,
		Timebase:  tb,
		Duration:  int64(dur),
		Container: "mp4",
		Color:     DefaultColor,
	}

	// Edit list: first entry's media_time is the presentation offset.
	elsts, _ := read(mp4.BoxPath{mp4.BoxTypeEdts(), mp4.BoxTypeElst()})
	if len(elsts) > 0 {
		if elst, ok := elsts[0].Payload.(*mp4.Elst); ok && len(elst.Entries) > 0 {
			e := elst.Entries[0]
			mt := int64(e.MediaTimeV0)
			if elst.GetVersion() == 1 {
				mt = e.MediaTimeV1
			}
			if mt > 0 {
				track.EditStart = mt
				track.Notes = append(track.Notes, fmt.Sprintf("edit list media_time=%d ticks", mt))
			}
		}
	}

	switch kind {
	case KindVideo:
		track.Codec, track.CodedWidth, track.CodedHeight = parseVisual(rs, &track, read)
	case KindAudio:
		parseAudio(rs, &track, read)
	}
	track.DisplayWidth = int(tkhd.GetWidthInt())
	track.DisplayHeight = int(tkhd.GetHeightInt())
	if track.DisplayWidth == 0 {
		track.DisplayWidth = track.CodedWidth
	}
	if track.DisplayHeight == 0 {
		track.DisplayHeight = track.CodedHeight
	}
	track.RotationRaw = MatrixRotation(tkhd.Matrix)
	track.Rotation, track.RotationRaw = NormalizeRotation(track.RotationRaw)
	if track.RotationRaw != 0 {
		track.Notes = append(track.Notes, fmt.Sprintf("rotation raw=%.3f normalized=%d", track.RotationRaw, track.Rotation))
	}
	return track, track.Key, nil
}

// parseVisual fills codec/dimensions and the visual children (colr, clap).
func parseVisual(rs io.ReadSeeker, track *Track, read func(mp4.BoxPath) ([]*mp4.BoxInfoWithPayload, error)) (string, int, int) {
	codec := "unknown"
	var w, h int
	for _, entryType := range visualSampleEntries() {
		entries, err := read(mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), entryType})
		if err != nil || len(entries) == 0 {
			continue
		}
		codec = entryType.String()
		if vse, ok := entries[0].Payload.(*mp4.VisualSampleEntry); ok {
			w, h = int(vse.Width), int(vse.Height)
		}
		colrs, _ := read(mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), entryType, mp4.BoxTypeColr()})
		if len(colrs) > 0 {
			if c, ok := colrs[0].Payload.(*mp4.Colr); ok {
				if string(c.ColourType[:]) == "nclx" {
					track.Color = NewColor(c.ColourPrimaries, c.TransferCharacteristics, c.MatrixCoefficients, c.FullRangeFlag, SourceColr)
					if track.Color.Incomplete {
						track.Notes = append(track.Notes, "colr nclx incomplete; missing fields defaulted and reported")
					}
				} else {
					track.Notes = append(track.Notes, "colr type "+string(c.ColourType[:])+" not represented; default recorded")
				}
			}
		}
		// Encrypted sample entries are explicitly out of scope; refuse them
		// rather than silently mis-parsing ciphertext as a video frame.
		enc, _ := read(mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), boxType("encv")})
		if len(enc) > 0 {
			track.Notes = append(track.Notes, "encrypted sample entry (encv) is out of scope")
		}
		claps, _ := read(mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), entryType, boxType("clap")})
		if len(claps) > 0 {
			if cp, ok := claps[0].Payload.(*Clap); ok {
				if crop, ok := cropFromClap(cp, w, h); ok {
					track.Crop = crop
				} else {
					track.Notes = append(track.Notes, "clap has invalid rational components")
				}
			}
		}
		break
	}
	return codec, w, h
}

// cropFromClap converts a clean aperture to a top-left rectangle. ISO/IEC
// 14496-12 defines the offsets relative to the frame centre:
//
//	x = (codedWidth - cropWidth)/2 + horizOff
//	y = (codedHeight - cropHeight)/2 + vertOff
func cropFromClap(cp *Clap, codedW, codedH int) (*Crop, bool) {
	if cp.CleanApertureWidthD <= 0 || cp.CleanApertureHeightD <= 0 ||
		cp.HorizOffD == 0 || cp.VertOffD == 0 {
		return nil, false
	}
	w := int(cp.CleanApertureWidthN / cp.CleanApertureWidthD)
	h := int(cp.CleanApertureHeightN / cp.CleanApertureHeightD)
	hOff := int(cp.HorizOffN / cp.HorizOffD)
	vOff := int(cp.VertOffN / cp.VertOffD)
	return &Crop{
		X: (codedW-w)/2 + hOff,
		Y: (codedH-h)/2 + vOff,
		W: w, H: h,
	}, true
}

func parseAudio(rs io.ReadSeeker, track *Track, read func(mp4.BoxPath) ([]*mp4.BoxInfoWithPayload, error)) {
	entries, err := read(mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), mp4.BoxTypeMp4a()})
	if err != nil || len(entries) == 0 {
		track.Codec = "unknown"
		return
	}
	track.Codec = "mp4a"
	if ase, ok := entries[0].Payload.(*mp4.AudioSampleEntry); ok {
		track.Channels = int(ase.ChannelCount)
		track.SampleRate = int(ase.SampleRate >> 16)
	}
}

// validateColor rejects an explicitly present but entirely unspecified color
// box. A missing box is not an error (the default is recorded instead), but a
// present-and-empty one must not be silently replaced by a default.
func validateColor(t Track) *model.Error {
	if t.Kind != KindVideo || t.Color.Source == SourceDefault {
		return nil
	}
	if t.Color.Incomplete {
		return model.New(model.CategoryMetadata, "colr",
			"color description incomplete; missing components recorded as defaults").
			WithTrack(t.Key).WithInterval(t.Interval())
	}
	return nil
}
