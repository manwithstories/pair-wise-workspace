package fixture

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"

	"mediad/internal/fault"
	"mediad/internal/manifest"
)

// Options controls fixture generation.
type Options struct {
	Root   string
	Seed   int64
	Damage string
}

// WrapSpec contains RTP wire values deliberately placed near wraparound.
type WrapSpec struct {
	ClockRate     int    `json:"clock_rate"`
	BaseSequence  uint16 `json:"base_sequence"`
	BaseTimestamp uint32 `json:"base_timestamp"`
	FrameTicks    uint32 `json:"frame_ticks"`
}

// Bundle describes generated paths and optional injected damage.
type Bundle struct {
	Root     string      `json:"root"`
	InputDir string      `json:"input_dir"`
	DASHPath string      `json:"dash_path"`
	HLSPath  string      `json:"hls_path"`
	Segments []string    `json:"segments"`
	Init     string      `json:"init"`
	Wrap     WrapSpec    `json:"wrap"`
	Damage   *DamageInfo `json:"damage,omitempty"`
}

// Generate writes a complete local fixture tree.
func Generate(opts Options) (*Bundle, error) {
	if opts.Root == "" {
		return nil, fault.New(fault.InvalidArgument, "", -1, "fixture root is required")
	}
	inputDir := filepath.Join(opts.Root, "input")
	segDir := filepath.Join(inputDir, "segments")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return nil, fault.New(fault.IO, segDir, -1, "create fixture directories: %v", err)
	}

	b := &Bundle{
		Root: opts.Root, InputDir: inputDir,
		DASHPath: filepath.Join(inputDir, "dash.mpd"),
		HLSPath:  filepath.Join(inputDir, "hls.m3u8"),
		Init:     filepath.Join(inputDir, "init-0.mp4"),
		Wrap:     DefaultWrapSpec(opts.Damage),
	}
	for i := 0; i < SegmentCount; i++ {
		p := filepath.Join(segDir, segmentName(i))
		b.Segments = append(b.Segments, p)
	}

	init := buildInitSegment()
	for i := 0; i < 3; i++ {
		p := filepath.Join(inputDir, "init-"+string(rune('0'+i))+".mp4")
		if err := os.WriteFile(p, init, 0o644); err != nil {
			return nil, fault.New(fault.IO, p, -1, "write init: %v", err)
		}
	}
	if err := os.WriteFile(b.DASHPath, []byte(writeDASH()), 0o644); err != nil {
		return nil, fault.New(fault.IO, b.DASHPath, -1, "write DASH: %v", err)
	}
	if err := os.WriteFile(b.HLSPath, []byte(writeHLS(opts.Damage == DamageMissingInit)), 0o644); err != nil {
		return nil, fault.New(fault.IO, b.HLSPath, -1, "write HLS: %v", err)
	}
	wrapData, _ := json.MarshalIndent(b.Wrap, "", "  ")
	wrapPath := filepath.Join(inputDir, "rtp-wrap.json")
	if err := os.WriteFile(wrapPath, wrapData, 0o644); err != nil {
		return nil, fault.New(fault.IO, wrapPath, -1, "write wrap spec: %v", err)
	}

	for i := 0; i < SegmentCount; i++ {
		data := buildMediaSegment(opts.Seed, i)
		if err := os.WriteFile(b.Segments[i], data, 0o644); err != nil {
			return nil, fault.New(fault.IO, b.Segments[i], -1, "write segment: %v", err)
		}
	}

	dmg, err := applyDamage(opts, b)
	if err != nil {
		return nil, err
	}
	b.Damage = dmg
	if dmg != nil {
		data, _ := json.MarshalIndent(dmg, "", "  ")
		if err := os.WriteFile(filepath.Join(opts.Root, "damage.json"), data, 0o644); err != nil {
			return nil, fault.New(fault.IO, opts.Root, -1, "write damage info: %v", err)
		}
	}
	return b, nil
}

func segmentName(index int) string {
	return "seg-" + twoDigits(index+1) + ".m4s"
}

func twoDigits(v int) string {
	if v < 10 {
		return "0" + string(rune('0'+v))
	}
	return string(rune('0'+v/10)) + string(rune('0'+v%10))
}

// DefaultWrapSpec places RTP fields close enough to limits to force wraparound.
func DefaultWrapSpec(damage string) WrapSpec {
	baseTS := uint32(math.MaxUint32 - 45_000)
	if damage == DamageWrapTS {
		baseTS = math.MaxUint32 - 900
	}
	return WrapSpec{
		ClockRate: VideoClockRate, BaseSequence: 65530,
		BaseTimestamp: baseTS, FrameTicks: 3000, // 30 fps at 90 kHz
	}
}

func applyDamage(opts Options, b *Bundle) (*DamageInfo, error) {
	switch opts.Damage {
	case "":
		return nil, nil
	case DamageMissingInit:
		return &DamageInfo{
			Kind: opts.Damage, Path: b.HLSPath, Code: string(fault.InitMissing),
			FirstBoxOffset: -1, BoxOffset: -1, ErrorOffset: -1,
		}, nil
	case DamageWrapTS:
		return &DamageInfo{Kind: opts.Damage, Code: "WRAP_EXPECTED", Path: b.HLSPath}, nil
	case DamageCorruptMDAT, DamageTruncatedMDAT, DamageTruncatedBox:
		path := b.Segments[0]
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fault.New(fault.IO, path, -1, "read for damage: %v", err)
		}
		boxes, err := manifest.ScanAllBoxes(path, data)
		if err != nil {
			return nil, err
		}
		var mdats []manifest.Box
		for _, bx := range boxes {
			if bx.Type == "mdat" {
				mdats = append(mdats, bx)
			}
		}
		if len(mdats) < 2 {
			return nil, fault.New(fault.MdatCorrupt, path, -1, "fixture has %d mdats", len(mdats))
		}
		switch opts.Damage {
		case DamageCorruptMDAT:
			m := mdats[0]
			data[m.PayloadOffset] = 0xff
			data[m.PayloadOffset+1] = 0xff
			data[m.PayloadOffset+2] = 0xff
			data[m.PayloadOffset+3] = 0xff
			if err := os.WriteFile(path, data, 0o644); err != nil {
				return nil, fault.New(fault.IO, path, m.PayloadOffset, "inject mdat corruption: %v", err)
			}
			return &DamageInfo{Kind: opts.Damage, Path: path, Code: string(fault.MdatCorrupt),
				FirstBoxOffset: 0, BoxOffset: m.Offset, ErrorOffset: m.PayloadOffset}, nil
		case DamageTruncatedMDAT:
			m := mdats[len(mdats)-1]
			newSize := m.PayloadOffset + 64
			if err := os.Truncate(path, newSize); err != nil {
				return nil, fault.New(fault.IO, path, newSize, "truncate mdat: %v", err)
			}
			return &DamageInfo{Kind: opts.Damage, Path: path, Code: string(fault.BoxTruncated),
				FirstBoxOffset: 0, BoxOffset: m.Offset, ErrorOffset: newSize}, nil
		case DamageTruncatedBox:
			if err := os.Truncate(path, 12); err != nil {
				return nil, fault.New(fault.IO, path, 12, "truncate first box: %v", err)
			}
			return &DamageInfo{Kind: opts.Damage, Path: path, Code: string(fault.BoxTruncated),
				FirstBoxOffset: 0, BoxOffset: 0, ErrorOffset: 12}, nil
		}
	}
	return nil, fault.New(fault.InvalidArgument, "", -1, "unknown damage kind %q", opts.Damage)
}
