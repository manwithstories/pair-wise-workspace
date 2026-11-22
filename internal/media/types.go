// Package media defines the version-neutral types exchanged between adapters,
// the mapping engine and the remux kernel. Adapters translate their container
// into these structures; nothing in the kernel knows about MP4 boxes or RTP
// headers directly.
package media

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// TrackKind classifies a logical track.
type TrackKind string

const (
	// KindVideo carries the primary picture track.
	KindVideo TrackKind = "video"
	// KindAudio carries the primary sound track.
	KindAudio TrackKind = "audio"
	// KindAux carries auxiliary tracks: timecode, captions, RTP data, etc.
	KindAux TrackKind = "aux"
)

// Valid reports whether k is a recognised kind.
func (k TrackKind) Valid() bool {
	return k == KindVideo || k == KindAudio || k == KindAux
}

// MediaType is the four-character sample-entry code (avc1, mp4a, tmcd, ...).
type MediaType string

// Codec identifies an encoding configuration. Config is an opaque, stable
// byte blob (e.g. avcC or esds payload) hashed into provenance.
type Codec struct {
	SampleEntry  MediaType `json:"sample_entry"`
	Config       []byte    `json:"-"`
	ConfigSHA256 string    `json:"config_sha256"`
	Width        uint32    `json:"width,omitempty"`
	Height       uint32    `json:"height,omitempty"`
	Channels     uint16    `json:"channels,omitempty"`
	SampleRate   uint32    `json:"sample_rate,omitempty"`
}

// Seal computes and stores the config digest.
func (c *Codec) Seal() {
	sum := sha256.Sum256(c.Config)
	c.ConfigSHA256 = hex.EncodeToString(sum[:])
}

// Track describes one demultiplexed track in the unified time base.
type Track struct {
	// Index is the adapter-local ordinal (0-based).
	Index int `json:"index"`
	// ID is the container track id (MP4 track_ID, or RTP SSRC).
	ID uint32 `json:"id"`
	// Kind is the logical role.
	Kind TrackKind `json:"kind"`
	// Timescale is the source units per second.
	Timescale uint32 `json:"timescale"`
	// TimescaleName is a human label (e.g. "mvhd", "rtp").
	TimescaleName string `json:"timescale_name"`
	// OriginNS is the local nanosecond instant subtracted to normalise the
	// track onto the asset timeline (non-zero-start and edit-list handling).
	OriginNS int64 `json:"origin_ns"`
	// DurationNS is the normalised duration after edits/discards.
	DurationNS int64 `json:"duration_ns"`
	// Codec carries the sample-entry description.
	Codec Codec `json:"codec"`
	// SampleEntryOffset is the byte offset of the sample entry in the source;
	// used for error locators.
	SampleEntryOffset int64 `json:"sample_entry_offset"`
	// PacketCount is the number of encoded packets exposed by the adapter.
	PacketCount int64 `json:"packet_count"`
}

// Packet is one encoded sample (MP4) or one RTP payload. Payload always points
// at the encoded bytes with no decoding applied. Data holds the raw bytes when
// the adapter materialised them; adapters should stream and leave Data nil and
// rely on ReadAt for large sources.
type Packet struct {
	// TrackIndex references Track.Index.
	TrackIndex int `json:"track_index"`
	// PacketIndex is the monotonic 0-based ordinal within the track.
	PacketIndex int64 `json:"packet_index"`
	// SourceOffset is the byte offset of the first byte of the packet.
	SourceOffset int64 `json:"source_offset"`
	// SourceSize is the encoded byte length.
	SourceSize int64 `json:"source_size"`
	// DTSNS is decode time on the asset timeline.
	DTSNS int64 `json:"dts_ns"`
	// PTSNS is presentation time on the asset timeline.
	PTSNS int64 `json:"pts_ns"`
	// DurationNS is the packet duration.
	DurationNS int64 `json:"duration_ns"`
	// Sync marks a random-access (keyframe) sample.
	Sync bool `json:"sync"`
	// Sequence is the source sequence number (RTP seq or MP4 sample number).
	Sequence uint32 `json:"sequence"`
	// Payload is the encoded payload bytes (optional).
	Payload []byte `json:"-"`
	// SHA256 is the digest of Payload, filled by the adapter or the kernel.
	SHA256 string `json:"sha256"`
}

// Digest computes and stores the payload digest.
func (p *Packet) Digest() string {
	sum := sha256.Sum256(p.Payload)
	p.SHA256 = hex.EncodeToString(sum[:])
	return p.SHA256
}

// Asset is the adapter output consumed by the kernel.
type Asset struct {
	ID         string  `json:"id"`
	SourcePath string  `json:"source_path"`
	Adapter    string  `json:"adapter"`
	Version    int     `json:"adapter_version"`
	SourceSHA  string  `json:"source_sha256"`
	Tracks     []Track `json:"tracks"`
	// Packets are ordered by (TrackIndex, PacketIndex) once retained.
	Packets []Packet `json:"-"`
	// DurationNS is the longest normalised track duration.
	DurationNS int64 `json:"duration_ns"`
	// Warnings are non-fatal adapter observations (sparse index, empty edit
	// list, ...) carried into the manifest.
	Warnings []string `json:"warnings,omitempty"`
}

// SortPackets orders packets stably by track then packet index.
func (a *Asset) SortPackets() {
	sort.SliceStable(a.Packets, func(i, j int) bool {
		if a.Packets[i].TrackIndex != a.Packets[j].TrackIndex {
			return a.Packets[i].TrackIndex < a.Packets[j].TrackIndex
		}
		return a.Packets[i].PacketIndex < a.Packets[j].PacketIndex
	})
}

// TrackByIndex returns the track with the given adapter index.
func (a *Asset) TrackByIndex(i int) (Track, bool) {
	for _, t := range a.Tracks {
		if t.Index == i {
			return t, true
		}
	}
	return Track{}, false
}

// TrackByID returns the track with the given container id.
func (a *Asset) TrackByID(id uint32) (Track, bool) {
	for _, t := range a.Tracks {
		if t.ID == id {
			return t, true
		}
	}
	return Track{}, false
}
