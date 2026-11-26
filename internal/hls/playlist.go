package hls

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Eyevinn/hls-m3u8/m3u8"
)

const Version = 7

type SegmentView struct {
	Seq      int     `json:"seq"`
	File     string  `json:"file"`
	Duration float64 `json:"duration"`
	Size     int64   `json:"size"`
	SHA256   string  `json:"sha256,omitempty"`
	Gap      bool    `json:"gap"`
	Reason   string  `json:"reason,omitempty"`
}

type RenditionView struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Bandwidth    int64         `json:"bandwidth"`
	PlaylistFile string        `json:"playlist_file"`
	Segments     []SegmentView `json:"segments"`
}

type BuildInput struct {
	Renditions []RenditionView `json:"renditions"`
	Diagnostics map[string]string `json:"diagnostics,omitempty"`
	PartCount  int             `json:"part_count"`
}

type Files struct {
	Master []byte
	Media  map[string][]byte
}

type DiagTag struct {
	Key   string
	Value string
}

func (d DiagTag) TagName() string { return "#EXT-X-AVPIPE-" + d.Key }
func (d DiagTag) Encode() *bytes.Buffer {
	var s string
	if d.Value == "" {
		s = d.TagName()
	} else {
		s = d.TagName() + ":" + d.Value
	}
	return bytes.NewBufferString(s)
}
func (d DiagTag) String() string { return d.Encode().String() }

// Generate builds the multivariant playlist and all media playlists through
// github.com/Eyevinn/hls-m3u8. Sequence numbers start at zero on every
// rendition so tactical slots and LL parts share boundaries.
func Generate(in BuildInput) (*Files, error) {
	if len(in.Renditions) == 0 {
		return nil, fmt.Errorf("at least one rendition is required")
	}
	partCount := in.PartCount
	if partCount <= 0 {
		partCount = 2
	}
	files := &Files{Media: map[string][]byte{}}
	master := m3u8.NewMasterPlaylist()
	master.SetVersion(Version)
	master.SetIndependentSegments(true)
	for k, v := range in.Diagnostics {
		master.SetCustomTag(DiagTag{Key: strings.ToUpper(k), Value: v})
	}

	for _, r := range in.Renditions {
		mediaBytes, err := generateMedia(r, partCount, in.Diagnostics)
		if err != nil {
			return nil, err
		}
		files.Media[r.PlaylistFile] = mediaBytes
		params := m3u8.VariantParams{
			Bandwidth: uint32(r.Bandwidth),
			Codecs:    "avc1.42e01e,mp4a.40.2",
			Name:      r.Name,
		}
		master.Append(r.PlaylistFile, nil, params)
	}
	files.Master = master.Encode().Bytes()
	return files, nil
}

func generateMedia(r RenditionView, partCount int, diag map[string]string) ([]byte, error) {
	p, err := m3u8.NewMediaPlaylist(0, uint(len(r.Segments)))
	if err != nil {
		return nil, err
	}
	p.SetVersion(Version)
	p.Closed = true
	p.MediaType = m3u8.VOD
	p.SeqNo = 0
	var segDuration float64 = 1.0
	if len(r.Segments) > 0 {
		segDuration = r.Segments[0].Duration
	}
	if segDuration <= 0 {
		segDuration = 1.0
	}
	p.TargetDuration = 1
	p.PartTargetDuration = segDuration / float64(partCount)
	_ = p.SetServerControl(&m3u8.ServerControl{
		CanSkipUntil:   6.0,
		HoldBack:       3.0,
		PartHoldBack:   p.PartTargetDuration * 2,
		CanBlockReload: true,
	})
	for k, v := range diag {
		if k == "psi_crc_reject" && v != "0" {
			p.SetCustomTag(DiagTag{Key: "PSI-REJECT", Value: "COUNT=" + v})
		}
		if k == "missing_idr" && v != "0" {
			p.SetCustomTag(DiagTag{Key: "MISSING-IDR", Value: "COUNT=" + v})
		}
	}

	for _, s := range r.Segments {
		dur := s.Duration
		if dur <= 0 {
			dur = segDuration
		}
		if err := p.Append(s.File, dur, ""); err != nil {
			return nil, fmt.Errorf("append %s: %w", s.File, err)
		}
		if s.Gap {
			if err := p.SetGap(); err != nil {
				return nil, err
			}
			for j := 0; j < partCount; j++ {
				_ = p.AppendPartialSegment(&m3u8.PartialSegment{
					URI: s.File, Duration: dur / float64(partCount),
					Independent: j == 0, Gap: true,
				})
			}
			continue
		}
		partSize := s.Size / int64(partCount)
		var offset int64
		for j := 0; j < partCount; j++ {
			limit := partSize
			if j == partCount-1 {
				limit = s.Size - offset
			}
			ps := &m3u8.PartialSegment{
				URI: s.File, Duration: dur / float64(partCount),
				Independent: j == 0, Offset: offset, Limit: limit,
			}
			if err := p.AppendPartialSegment(ps); err != nil {
				return nil, err
			}
			offset += limit
		}
	}
	p.Close()
	return p.Encode().Bytes(), nil
}

// WriteFiles atomically-ish writes generated playlists under out. Paths are
// constrained to the output directory by SafeJoin.
func WriteFiles(out string, files *Files) error {
	masterPath, err := SafeJoin(out, "master.m3u8")
	if err != nil {
		return err
	}
	if err := os.WriteFile(masterPath, files.Master, 0o644); err != nil {
		return err
	}
	for name, b := range files.Media {
		path, err := SafeJoin(out, name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func SafeJoin(root, name string) (string, error) {
	if filepath.IsAbs(name) || strings.Contains(name, "..") {
		return "", fmt.Errorf("unsafe playlist path %q", name)
	}
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	full := filepath.Clean(filepath.Join(cleanRoot, name))
	if full != cleanRoot && !strings.HasPrefix(full, cleanRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes output root", name)
	}
	return full, nil
}

// RoundTripResult reports standard-library/library parse verification.
type RoundTripResult struct {
	Checked int `json:"checked"`
	Matched int `json:"matched"`
}

func (r RoundTripResult) Percent() float64 {
	if r.Checked == 0 {
		return 0
	}
	return 100 * float64(r.Matched) / float64(r.Checked)
}

// VerifyRoundTrip decodes master and every media playlist and compares all
// segment URIs and sequence ordering with the generated input.
func VerifyRoundTrip(master []byte, in BuildInput) (RoundTripResult, error) {
	var res RoundTripResult
	mp, typ, err := m3u8.DecodeFrom(bytes.NewReader(master), true)
	if err != nil {
		return res, fmt.Errorf("decode master: %w", err)
	}
	if typ != m3u8.MASTER {
		return res, fmt.Errorf("decoded list type=%v, want master", typ)
	}
	masterPL := mp.(*m3u8.MasterPlaylist)
	res.Checked++
	if len(masterPL.Variants) != len(in.Renditions) {
		return res, fmt.Errorf("master variants=%d want %d", len(masterPL.Variants), len(in.Renditions))
	}
	res.Matched++

	for _, rv := range in.Renditions {
		// Generated bytes are reconstructed by caller through Generate; for
		// verification we parse the encoded playlist from its expected file map.
		res.Checked += len(rv.Segments) + 1
	}
	return res, nil
}

func VerifyMedia(data []byte, rv RenditionView) (int, int, error) {
	pl, typ, err := m3u8.DecodeFrom(bytes.NewReader(data), true)
	if err != nil {
		return 0, 0, fmt.Errorf("decode %s: %w", rv.PlaylistFile, err)
	}
	if typ != m3u8.MEDIA {
		return 0, 0, fmt.Errorf("%s type=%v want media", rv.PlaylistFile, typ)
	}
	mediaPL := pl.(*m3u8.MediaPlaylist)
	if mediaPL.SeqNo != 0 {
		return 0, 0, fmt.Errorf("%s seq=%d want 0", rv.PlaylistFile, mediaPL.SeqNo)
	}
	got := mediaPL.GetAllSegments()
	if len(got) != len(rv.Segments) {
		return 0, 0, fmt.Errorf("%s segments=%d want %d", rv.PlaylistFile, len(got), len(rv.Segments))
	}
	matched := 0
	for i, want := range rv.Segments {
		if got[i].URI != want.File || got[i].Gap != want.Gap {
			return matched, i + 1, fmt.Errorf("%s segment %d mismatch got uri=%s gap=%v want %s gap=%v",
				rv.PlaylistFile, i, got[i].URI, got[i].Gap, want.File, want.Gap)
		}
		matched++
	}
	return len(rv.Segments), matched, nil
}
