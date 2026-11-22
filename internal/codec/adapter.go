// Package codec defines the versioned codec-metadata adapter contract.
//
// An adapter inspects one container sample entry and reports the audio
// parameters the media kernel needs: sample rate, channel count, sample
// width, interleaving and any codec-specific configuration. Decoding is
// deliberately minimal: this build normalises linear PCM, and recognises
// AAC configurations well enough to describe them and to fail with a
// precise error instead of guessing.
package codec

import (
	"errors"

	"mediacore/internal/media"
)

// ErrUnsupported is returned when no adapter recognises a sample entry.
var ErrUnsupported = errors.New("codec: unsupported sample entry")

// Version identifies a codec adapter revision bound into checkpoints.
type Version struct {
	Name  string
	Major int
	Minor int
}

// String renders a stable identifier such as "pcm/1.0".
func (v Version) String() string {
	return v.Name + "/" + itoa(v.Major) + "." + itoa(v.Minor)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [12]byte
	p := len(buf)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		p--
		buf[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		buf[p] = '-'
	}
	return string(buf[p:])
}

// Mapping identifies the byte layout of a sample.
type Mapping string

const (
	// MappingS16LE is signed 16-bit little-endian, interleaved.
	MappingS16LE Mapping = "s16le"
	// MappingS16BE is signed 16-bit big-endian, interleaved.
	MappingS16BE Mapping = "s16be"
	// MappingCompressed marks codec-compressed frames (e.g. AAC).
	MappingCompressed Mapping = "compressed"
)

// Format is the normalised description of one audio track's samples.
type Format struct {
	// Codec is a stable identifier: "pcm" or "aac".
	Codec string
	// Mapping tells the engine how sample bytes become frames.
	Mapping Mapping
	// SampleRate and Channels describe the input.
	SampleRate int
	Channels   int
	// BitsPerSample is the container's declared width (16 for PCM).
	BitsPerSample int
	// FrameBytes is one interleaved frame (channels * bytes/sample) for
	// uncompressed layouts, or 0 for compressed.
	FrameBytes int
	// Config carries codec-specific bytes (AAC AudioSpecificConfig).
	Config []byte
	// ObjectType is the codec profile where one is defined (AAC AOT).
	ObjectType int
}

// Description is the adapter's verdict for a sample entry.
type Description struct {
	Format   Format
	Adapter  Version
	Warnings []string
}

// Adapter inspects a sample entry and optionally decodes one sample.
type Adapter interface {
	Version() Version
	// Supports reports whether the four-character sample-entry tag is one
	// this adapter understands.
	Supports(tag string, config []byte) bool
	// Describe validates the entry and returns normalised parameters.
	Describe(tag string, config []byte, rate, channels, bits int) (Description, error)
	// Decode converts one container sample into interleaved float64
	// frames in [-1, 1]. dst must hold at least frames*channels values.
	// For compressed formats Decode returns ErrUnsupported.
	Decode(sample []byte, f Format, dst []float64) (frames int, err error)
}

// Registry is an ordered set of adapters. First match wins, so a more
// specific adapter can be registered ahead of a generic one.
type Registry struct {
	adapters []Adapter
}

// NewRegistry builds a registry from adapters.
func NewRegistry(adapters ...Adapter) *Registry {
	return &Registry{adapters: adapters}
}

// Default returns the registry used by the command layer.
func Default() *Registry {
	return NewRegistry(&PCM{}, &AAC{})
}

// Describe finds the first adapter that supports tag and describes it.
func (r *Registry) Describe(tag string, config []byte, rate, channels, bits int) (Description, error) {
	for _, a := range r.adapters {
		if !a.Supports(tag, config) {
			continue
		}
		return a.Describe(tag, config, rate, channels, bits)
	}
	return Description{}, media.NewError(media.CodeUnsupportedCodec, -1, 0,
		"unsupported audio sample entry %q", tag)
}

// Decode uses the adapter matching f.Codec and f.Mapping.
func (r *Registry) Decode(sample []byte, f Format, dst []float64) (int, error) {
	for _, a := range r.adapters {
		desc, err := a.Describe(codecTagFor(f), f.Config, f.SampleRate, f.Channels, f.BitsPerSample)
		if err != nil {
			continue
		}
		if desc.Format.Codec == f.Codec {
			return a.Decode(sample, f, dst)
		}
	}
	return 0, media.NewError(media.CodeUnsupportedCodec, -1, 0, "no decoder for %q", f.Codec)
}

func codecTagFor(f Format) string {
	if f.Codec == "aac" {
		return "mp4a"
	}
	switch f.Mapping {
	case MappingS16LE:
		return "sowt"
	case MappingS16BE:
		return "twos"
	}
	return "lpcm"
}
