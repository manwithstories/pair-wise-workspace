package ingest

import (
	"strings"

	"github.com/example/mediad/internal/model"
)

// mxfToSummary maps the decoded KLV fixture onto the shared Summary
// model and applies the same raw/adjudicated split as the MP4 path.
func mxfToSummary(f *MXFFile, path string) (*Summary, error) {
	if _, ok := findField(f.HeaderFields, MXFTagCorruptMarker); ok {
		return nil, model.New(model.KindCorrupt, "ingest", "mxf-parse",
			path+": corrupt marker present in preface")
	}
	containerRot := Rotation(0)
	var containerCrop *Crop
	var containerColor *Color
	if v, ok := findField(f.HeaderFields, MXFTagRotation); ok {
		containerRot = Rotation(v.U32)
	}
	if v, ok := findField(f.HeaderFields, MXFTagCrop); ok {
		c := v.Crop
		containerCrop = &c
	}
	if c, ok := mxfColor(f.HeaderFields, "container"); ok {
		containerColor = c
	}

	sum := &Summary{Format: "mxf", Path: path}
	for _, mt := range f.Tracks {
		isPicture, _ := findField(mt.Fields, MXFTagPictureEssence)
		isSound, _ := findField(mt.Fields, MXFTagSoundEssence)
		switch {
		case isPicture != nil:
			tr := buildMXFVideo(mt, containerRot, containerCrop, containerColor)
			sum.Tracks = append(sum.Tracks, tr)
		case isSound != nil:
			tr := buildMXFAudio(mt)
			sum.Tracks = append(sum.Tracks, tr)
		}
	}
	if len(sum.Tracks) == 0 {
		return nil, model.New(model.KindCorrupt, "ingest", "mxf-parse",
			path+": no essence tracks")
	}
	return sum, nil
}

func buildMXFVideo(mt MXFTrack, cRot Rotation, cCrop *Crop, cColor *Color) *Track {
	v := &VideoTrackSummary{}
	if f, ok := findField(mt.Fields, MXFTagTrackID); ok {
		v.TrackID = f.U32
	}
	v.Codec = "h264"
	if f, ok := findField(mt.Fields, MXFTagWidth); ok {
		v.Width = f.U32
	}
	if f, ok := findField(mt.Fields, MXFTagHeight); ok {
		v.Height = f.U32
	}
	if f, ok := findField(mt.Fields, MXFTagDuration); ok {
		v.SampleCount = f.U64
	}
	if f, ok := findField(mt.Fields, MXFTagKeyframeIndex); ok {
		for i := 0; i+4 <= len(f.Bytes); i += 4 {
			v.KeyframeIndexes = append(v.KeyframeIndexes,
				uint32(f.Bytes[i])<<24|uint32(f.Bytes[i+1])<<16|
					uint32(f.Bytes[i+2])<<8|uint32(f.Bytes[i+3]))
		}
	}
	if f, ok := findField(mt.Fields, MXFTagCrop); ok {
		c := f.Crop
		v.Crop = &c
	}
	if c, ok := mxfColor(mt.Fields, "track"); ok {
		v.Colors = append(v.Colors, *c)
	}
	if cColor != nil {
		v.Colors = append([]Color{*cColor}, v.Colors...)
	}
	trackRot := Rotation(0)
	if f, ok := findField(mt.Fields, MXFTagRotation); ok {
		trackRot = Rotation(f.U32)
	}
	return &Track{
		Kind:          "video",
		Video:         v,
		ContainerRot:  cRot,
		TrackRot:      trackRot,
		ContainerCrop: cCrop,
	}
}

func buildMXFAudio(mt MXFTrack) *Track {
	a := &AudioTrackLayout{Codec: "pcm"}
	if f, ok := findField(mt.Fields, MXFTagTrackID); ok {
		a.TrackID = f.U32
	}
	if f, ok := findField(mt.Fields, MXFTagSampleRate); ok {
		a.SampleRate = f.U32
	}
	if f, ok := findField(mt.Fields, MXFTagChannels); ok {
		a.Channels = uint16(f.U32)
	}
	if f, ok := findField(mt.Fields, MXFTagChannelLayout); ok {
		a.ChannelLayout = splitLayout(f.Str)
	}
	if f, ok := findField(mt.Fields, MXFTagDuration); ok {
		a.SampleCount = f.U64
	}
	return &Track{Kind: "audio", Audio: a}
}

func splitLayout(s string) []string {
	parts := strings.Split(s, "\x00")
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func mxfColor(fields []mxfField, source string) (*Color, bool) {
	p, okP := findField(fields, MXFTagColorPrimaries)
	if !okP {
		return nil, false
	}
	c := &Color{Source: source, Primaries: uint16(p.U32)}
	if f, ok := findField(fields, MXFTagColorTransfer); ok {
		c.Transfer = uint16(f.U32)
	}
	if f, ok := findField(fields, MXFTagColorMatrix); ok {
		c.Matrix = uint16(f.U32)
	}
	if _, ok := findField(fields, MXFTagColorICC); ok {
		c.ICCProfile = true
	}
	return c, true
}
