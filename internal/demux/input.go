// Package demux opens local MXF and MP4 inputs, exposes their tracks and
// packets, and performs the explicit track mapping the pipeline requires.
//
// Nothing here decodes media: packets are handed to the pipeline as encoded
// bytes with their container timing intact. Demuxing is where corruption,
// illegal geometry and missing timestamps are first detected, and where every
// reported failure is anchored to a byte offset in the source file.
package demux

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/asticode/go-astiav"

	"avpipe/internal/media"
	"avpipe/internal/metadata"
)

// Limits bound resource use per the acceptance contract. Zero fields take the
// documented defaults.
type Limits struct {
	MaxTracks        int   // default 8
	MaxPacketBytes   int   // default 64 MiB
	MaxPackets       int64 // default 4,000,000
	MaxFileBytes     int64 // default 4 GiB
	MaxDurationTicks int64 // default 0 (unlimited)
}

// DefaultLimits returns the documented defaults.
func DefaultLimits() Limits {
	return Limits{
		MaxTracks:      8,
		MaxPacketBytes: 64 << 20,
		MaxPackets:     4_000_000,
		MaxFileBytes:   4 << 30,
	}
}

// normalize fills zero fields with defaults and rejects nonsense.
func (l Limits) normalize() (Limits, error) {
	if l.MaxTracks < 0 || l.MaxPacketBytes < 0 || l.MaxPackets < 0 || l.MaxFileBytes < 0 || l.MaxDurationTicks < 0 {
		return l, media.Errorf(media.KindUsage, media.Origin{}, "demux.Limits", "limits must not be negative")
	}
	if l.MaxTracks == 0 {
		l.MaxTracks = 8
	}
	if l.MaxPacketBytes == 0 {
		l.MaxPacketBytes = 64 << 20
	}
	if l.MaxPackets == 0 {
		l.MaxPackets = 4_000_000
	}
	if l.MaxFileBytes == 0 {
		l.MaxFileBytes = 4 << 30
	}
	return l, nil
}

// Container identifies the demuxed container family.
type Container string

const (
	ContainerMXF Container = "mxf"
	ContainerMP4 Container = "mp4"
)

// Input is one opened local media file.
type Input struct {
	Path      string
	Container Container
	Format    string // libavformat demuxer name
	Tracks    []media.Track
	Limits    Limits

	fc       *astiav.FormatContext
	packet   *astiav.Packet
	scratch  map[int]*trackState
	closed   bool
	read     int64
	// containerFacts holds one entry per trak/tkhd, in file order. FFmpeg
	// assigns stream indices in the same order, so index i describes stream i.
	containerFacts []containerFacts
}

// trackState accumulates per-stream unwrapping and ordering state.
type trackState struct {
	track     media.Track
	seq       int64
	arrival   int64
	unwrapper UnwrapperState
}

// UnwrapperState is an interface so tests can inject a deterministic clock.
type UnwrapperState interface {
	Value(raw int64) (int64, error)
	Wraps() int
}

// Options control opening an input.
type Options struct {
	Limits Limits
	// WrapBits is applied to every track's DTS/PTS. 0 disables unwrapping.
	WrapBits int
	// ReadTimeout bounds a single demux read in Go duration form; zero means
	// no bound.
	ReadTimeoutTicks int64
	// Explicit display configuration applied during track construction.
	Explicit metadata.Explicit
}

// ErrNoUsableTracks is returned when a file opens but yields no video/audio.
var ErrNoUsableTracks = errors.New("demux: no usable audio or video tracks")

// Open opens a local MXF or MP4 file and builds its tracks. Paths must be
// local regular files; URLs, devices and symlinks that escape are rejected.
func Open(path string, opts Options) (*Input, error) {
	lim, err := opts.Limits.normalize()
	if err != nil {
		return nil, err
	}
	abs, err := validatePath(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, media.New(media.KindUsage, media.Origin{File: abs}, "demux.Open", "stat failed", err)
	}
	if !info.Mode().IsRegular() {
		return nil, media.Errorf(media.KindUsage, media.Origin{File: abs}, "demux.Open", "not a regular file")
	}
	if info.Size() <= 0 {
		return nil, media.Errorf(media.KindCorrupt, media.Origin{File: abs}, "demux.Open", "file is empty")
	}
	if info.Size() > lim.MaxFileBytes {
		return nil, media.Errorf(media.KindUsage, media.Origin{File: abs}, "demux.Open",
			"file size %d exceeds limit %d", info.Size(), lim.MaxFileBytes)
	}

	// Never let libavformat log to stderr during tests or selfcheck; a later
	// caller may install its own callback.
	astiav.SetLogLevel(astiav.LogLevelQuiet)

	fc := astiav.AllocFormatContext()
	if fc == nil {
		return nil, media.New(media.KindInternal, media.Origin{File: abs}, "demux.Open", "allocating format context failed", nil)
	}
	if err := fc.OpenInput(abs, nil, nil); err != nil {
		fc.Free()
		return nil, media.New(media.KindCorrupt, media.Origin{File: abs}, "demux.Open",
			"opening container failed (truncated or unsupported)", err)
	}
	if err := fc.FindStreamInfo(nil); err != nil {
		fc.CloseInput()
		fc.Free()
		return nil, media.New(media.KindCorrupt, media.Origin{File: abs}, "demux.Open",
			"reading stream info failed (damaged header)", err)
	}
	in := &Input{
		Path: abs, Limits: lim,
		fc: fc, packet: astiav.AllocPacket(),
		scratch: map[int]*trackState{},
	}
	in.Container = classifyContainer(fc.InputFormat().Name())
	// Only MXF and MP4 are supported; anything else is rejected up front.
	if in.Container == "" {
		in.Close()
		return nil, media.Errorf(media.KindCorrupt, media.Origin{File: abs}, "demux.Open",
			"unsupported container %q (only mxf and mp4 are accepted)", fc.InputFormat().Name())
	}
	// Container-declared display facts are read from the MP4 box tree before
	// tracks are built so they can take part in metadata adjudication. MXF
	// carries its display metadata in the descriptor set, which FFmpeg
	// already reflects in codec parameters, so no extra parse is needed.
	if in.Container == ContainerMP4 {
		facts, err := readMP4ContainerFacts(abs, lim.MaxFileBytes)
		if err != nil {
			in.Close()
			return nil, err
		}
		in.containerFacts = facts
	}
	if err := in.buildTracks(opts); err != nil {
		in.Close()
		return nil, err
	}
	return in, nil
}

func classifyContainer(name string) Container {
	switch {
	case strings.Contains(name, "mxf"):
		return ContainerMXF
	case strings.Contains(name, "mp4"), strings.Contains(name, "mov"):
		return ContainerMP4
	default:
		return ""
	}
}

func validatePath(path string) (string, error) {
	if path == "" {
		return "", media.Errorf(media.KindUsage, media.Origin{}, "demux.Open", "empty path")
	}
	if strings.Contains(path, "://") || strings.HasPrefix(path, "-") {
		return "", media.Errorf(media.KindUsage, media.Origin{File: path}, "demux.Open",
			"only local filesystem paths are accepted")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", media.New(media.KindUsage, media.Origin{File: path}, "demux.Open", "resolving path failed", err)
	}
	// Refuse paths that climb out through ".." segments after cleaning.
	clean := filepath.Clean(abs)
	if strings.Contains(clean, ".."+string(filepath.Separator)+"..") {
		return "", media.Errorf(media.KindUsage, media.Origin{File: path}, "demux.Open", "path escapes are not allowed")
	}
	return clean, nil
}

func (in *Input) buildTracks(opts Options) error {
	streams := in.fc.Streams()
	usable := 0
	for _, s := range streams {
		cp := s.CodecParameters()
		var kind media.TrackKind
		switch cp.MediaType() {
		case astiav.MediaTypeVideo:
			kind = media.TrackVideo
		case astiav.MediaTypeAudio:
			kind = media.TrackAudio
		default:
			continue
		}
		usable++
		if usable > in.Limits.MaxTracks {
			return media.Errorf(media.KindPolicy, media.Origin{File: in.Path}, "demux.buildTracks",
				"track count exceeds limit %d", in.Limits.MaxTracks)
		}
		tb := rationalFrom(s.TimeBase())
		if !tb.Valid() || tb.Num == 0 {
			return media.Errorf(media.KindCorrupt, media.Origin{File: in.Path, TrackID: trackID(s, kind)},
				"demux.buildTracks", "stream %d has invalid time base", s.Index())
		}
		t := media.Track{
			ID: trackID(s, kind), Source: s.Index(), Kind: kind,
			Codec: codecName(cp), TimeBase: tb,
			TimestampBits: opts.WrapBits,
		}
		if kind == media.TrackVideo {
			t.DeclaredWidth = cp.Width()
			t.DeclaredHeight = cp.Height()
			di, err := in.videoDisplay(s, cp, t.ID, opts)
			if err != nil {
				// An illegal declared value invalidates the track: record it
				// but keep the track so mapping can report it explicitly.
				if me, ok := media.AsError(err); ok && me.Kind == media.KindCorrupt {
					t.Display = di
					t.Display.Invalid = true
					t.Display.InvalidReason = me.Msg
					in.scratch[s.Index()] = &trackState{track: t}
					in.Tracks = append(in.Tracks, t)
					continue
				}
				return err
			}
			t.Display = di
		}
		state := &trackState{track: t}
		if opts.WrapBits > 0 {
			us, err := NewUnwrapper(opts.WrapBits, media.Origin{File: in.Path, TrackID: t.ID})
			if err != nil {
				return err
			}
			state.unwrapper = us
		}
		in.scratch[s.Index()] = state
		in.Tracks = append(in.Tracks, t)
	}
	if usable == 0 {
		return media.New(media.KindCorrupt, media.Origin{File: in.Path}, "demux.buildTracks", ErrNoUsableTracks.Error(), ErrNoUsableTracks)
	}
	return nil
}

// trackID builds a stable identifier from kind plus stream order, never from
// the volatile container index. Video sorts before audio.
func trackID(s *astiav.Stream, kind media.TrackKind) string {
	prefix := "v"
	if kind == media.TrackAudio {
		prefix = "a"
	}
	return fmt.Sprintf("%s%d", prefix, s.Index())
}

func codecName(cp *astiav.CodecParameters) string {
	if name := cp.CodecID().Name(); name != "" {
		return strings.ToLower(name)
	}
	return "unknown"
}

func rationalFrom(r astiav.Rational) media.Rational {
	if r.Den() == 0 || r.Num() == 0 {
		return media.Rational{}
	}
	out, err := media.NewRat(int64(r.Num()), int64(r.Den()))
	if err != nil {
		return media.Rational{}
	}
	return out
}

// videoDisplay collects display facts from the stream and applies the
// metadata precedence rules.
func (in *Input) videoDisplay(s *astiav.Stream, cp *astiav.CodecParameters, id string, opts Options) (media.DisplayInfo, error) {
	facts := metadata.VideoFacts{
		TrackID: id, Width: cp.Width(), Height: cp.Height(),
		Origin: media.Origin{File: in.Path, TrackID: id, Stage: "demux"},
	}
	// Container-declared display facts (MP4 tkhd), when present for this
	// stream, are lower priority than the codec parameters.
	if s.Index() < len(in.containerFacts) {
		attachContainerFacts(&facts, in.containerFacts[s.Index()])
	}
	// Rotation from a display matrix side data, if present.
	if rot, ok := rotationOf(cp); ok {
		facts.HasTrackRot = true
		facts.TrackRotation = rot
	}
	// Color from codec parameters is the track-level declaration. A codec
	// bitstream declaration (e.g. an H.264 VUI) is lower priority and supplied
	// by the container/bitstream reader in a later stage.
	facts.Color = append(facts.Color, metadata.ColorSource{
		Source: metadata.SourceTrack,
		Color: media.ColorInfo{
			Primaries: colorPrimariesName(cp.ColorPrimaries()),
			Transfer:  colorTransferName(cp.ColorTransferCharacteristic()),
			Space:     colorSpaceName(cp.ColorSpace()),
			Range:     colorRangeName(cp.ColorRange()),
		},
	})
	return metadata.Resolve(facts, opts.Explicit)
}

// Packet reads the next encoded packet. It returns io.EOF at end of stream.
// Corruption, missing timestamps and backwards jumps surface here as typed
// errors anchored to the file offset of the offending packet.
func (in *Input) Packet() (media.Packet, error) {
	if in.closed {
		return media.Packet{}, media.Errorf(media.KindInternal, media.Origin{File: in.Path}, "demux.Packet", "input is closed")
	}
	if in.read >= in.Limits.MaxPackets {
		return media.Packet{}, media.Errorf(media.KindPolicy, media.Origin{File: in.Path}, "demux.Packet",
			"packet count exceeds limit %d", in.Limits.MaxPackets)
	}
	in.packet.Unref()
	if err := in.fc.ReadFrame(in.packet); err != nil {
		if errors.Is(err, astiav.ErrEof) {
			return media.Packet{}, io.EOF
		}
		return media.Packet{}, media.New(media.KindCorrupt, media.Origin{File: in.Path, Offset: -1},
			"demux.Packet", "reading frame failed", err)
	}
	idx := in.packet.StreamIndex()
	state, ok := in.scratch[idx]
	if !ok {
		// Not a mapped stream (e.g. data track): skip it.
		return media.Packet{}, media.Errorf(media.KindPolicy, media.Origin{File: in.Path},
			"demux.Packet", "packet for unmapped stream %d", idx)
	}
	size := in.packet.Size()
	if size < 0 || size > in.Limits.MaxPacketBytes {
		return media.Packet{}, media.Errorf(media.KindCorrupt,
			media.Origin{File: in.Path, Offset: in.packet.Pos(), TrackID: state.track.ID},
			"demux.Packet", "packet size %d exceeds limit %d", size, in.Limits.MaxPacketBytes)
	}
	p := media.Packet{
		TrackID: state.track.ID, Seq: state.seq, Arrival: state.arrival,
		TimeBase: state.track.TimeBase,
		RawPTS:   in.packet.Pts(), RawDTS: in.packet.Dts(),
		Duration: in.packet.Duration(),
		Key:      in.packet.Flags().Has(astiav.PacketFlagKey),
		Corrupt:  in.packet.Flags().Has(astiav.PacketFlagCorrupt),
		Origin:   media.Origin{File: in.Path, Offset: in.packet.Pos(), TrackID: state.track.ID, PacketSeq: state.seq},
	}
	if p.Corrupt {
		return media.Packet{}, media.Errorf(media.KindCorrupt, p.Origin, "demux.Packet",
			"packet is flagged corrupt by the container")
	}
	data := in.packet.Data()
	p.Data = make([]byte, len(data))
	copy(p.Data, data)

	// Timestamps: PTS and DTS must both be present, else isolate: this packet
	// is rejected rather than guessed.
	if in.packet.Pts() == astiav.NoPtsValue && in.packet.Dts() == astiav.NoPtsValue {
		return media.Packet{}, media.Errorf(media.KindCorrupt, p.Origin, "demux.Packet",
			"packet has neither PTS nor DTS")
	}
	p.PTS = tsOrMissing(in.packet.Pts())
	p.DTS = tsOrMissing(in.packet.Dts())

	// Unwrap the decode timestamp race when configured.
	if state.unwrapper != nil {
		if p.HasDTS() {
			v, err := state.unwrapper.Value(p.RawDTS)
			if err != nil {
				return media.Packet{}, wrapUnwrapErr(err, p.Origin)
			}
			p.DTS = v
		}
	}
	state.seq++
	state.arrival = in.read
	in.read++
	return p, nil
}

func tsOrMissing(v int64) int64 {
	if v == astiav.NoPtsValue {
		return media.NoTimestamp
	}
	return v
}

func wrapUnwrapErr(err error, origin media.Origin) error {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return media.Errorf(media.KindCorrupt, origin, "demux.Packet",
			"%s", ve.Message)
	}
	if me, ok := media.AsError(err); ok {
		return me.WithOrigin(origin)
	}
	return media.New(media.KindInternal, origin, "demux.Packet", "unwrapping timestamp failed", err)
}

// Close releases all native resources. It is safe to call twice.
func (in *Input) Close() {
	if in == nil || in.closed {
		return
	}
	in.closed = true
	if in.packet != nil {
		in.packet.Free()
		in.packet = nil
	}
	if in.fc != nil {
		in.fc.CloseInput()
		in.fc.Free()
		in.fc = nil
	}
}
