package fixtures

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"

	mp4 "github.com/abema/go-mp4"
)

// Media constants. Video runs at 25 fps on a 90 kHz clock; audio is 48 kHz.
const (
	videoTimescale = 90000
	videoFrameTick = 3600 // 40 ms
	audioTimescale = 48000
	audioFrame     = 1024
	movieTimescale = 1000
)

// boxWriter tracks nesting so callers can assert balanced open/close calls.
type boxWriter struct {
	w     *mp4.Writer
	depth int
}

func (b *boxWriter) open(typ string) error {
	_, err := b.start(typ)
	return err
}

func (b *boxWriter) start(typ string) (*mp4.BoxInfo, error) {
	bi, err := b.w.StartBox(&mp4.BoxInfo{Type: mp4.StrToBoxType(typ)})
	if err != nil {
		return nil, err
	}
	b.depth++
	return bi, nil
}

func (b *boxWriter) end() error {
	if b.depth == 0 {
		return fmt.Errorf("fixtures: unbalanced box close")
	}
	if _, err := b.w.EndBox(); err != nil {
		return err
	}
	b.depth--
	return nil
}

func (b *boxWriter) put(box mp4.IImmutableBox) error {
	_, err := mp4.Marshal(b.w, box, mp4.Context{})
	return err
}

// fullBox writes a leaf box with a frozen FullBox version.
func (b *boxWriter) leaf(typ string, box mp4.IImmutableBox, version uint8) error {
	if _, err := b.start(typ); err != nil {
		return err
	}
	if setter, ok := box.(interface{ SetVersion(uint8) }); ok {
		setter.SetVersion(version)
	}
	if err := b.put(box); err != nil {
		return err
	}
	return b.end()
}

func (b *boxWriter) container(typ string, children func() error) error {
	if _, err := b.start(typ); err != nil {
		return err
	}
	if err := children(); err != nil {
		return err
	}
	return b.end()
}

// trackPlan describes one track to synthesise.
type trackPlan struct {
	kind         string
	trackID      uint32
	timescale    uint32
	count        int
	delta        uint32
	keyEvery     int
	emptyEdits   []mp4.ElstEntry // optional prefixed empty edits
	payloadBytes int             // fixed sample payload size; 0 means vary
	pcm          bool            // fill payload with interleaved stereo PCM
}

// buildTrack renders a self-contained single-track MP4.
func buildTrack(rng *rand.Rand, plan trackPlan) ([]byte, error) {
	sizes := make([]uint32, plan.count)
	payloads := make([][]byte, plan.count)
	for i := 0; i < plan.count; i++ {
		n := plan.payloadBytes
		if n <= 0 {
			n = 64 + rng.Intn(97)
		}
		payloads[i] = make([]byte, n)
		if plan.pcm {
			fillPCM(payloads[i], i*n/4)
		} else if _, err := rng.Read(payloads[i]); err != nil {
			return nil, err
		}
		sizes[i] = uint32(n)
	}
	tmp, err := os.CreateTemp("", "mediaroute-fixture-*.mp4")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	defer os.Remove(name)

	stcoEntry, err := writeMovie(tmp, plan, sizes)
	if err != nil {
		tmp.Close()
		return nil, err
	}
	start, err := tmp.Seek(0, io.SeekCurrent)
	if err != nil {
		tmp.Close()
		return nil, err
	}
	offsets := make([]uint32, plan.count)
	cur := start
	for i, p := range payloads {
		offsets[i] = uint32(cur)
		cur += int64(len(p))
	}
	if err := writeMdat(tmp, payloads); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := patchStco(tmp, stcoEntry, offsets); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(name)
}

// writeMovie writes ftyp+moov and returns the offset of the first stco entry.
func writeMovie(f *os.File, plan trackPlan, sizes []uint32) (int64, error) {
	b := &boxWriter{w: mp4.NewWriter(f)}
	if err := b.leaf("ftyp", &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 512,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
			{CompatibleBrand: [4]byte{'i', 's', 'o', '2'}},
		},
	}, 0); err != nil {
		return 0, err
	}
	durationMS := uint32(int64(plan.count)*int64(plan.delta)*1000/int64(plan.timescale)) + 500
	var stcoEntry int64
	err := b.container("moov", func() error {
		if err := b.leaf("mvhd", &mp4.Mvhd{
			Timescale:   movieTimescale,
			DurationV0:  durationMS,
			Rate:        0x10000,
			NextTrackID: plan.trackID + 1,
		}, 0); err != nil {
			return err
		}
		return b.container("trak", func() error {
			if err := writeTkhd(b, plan, durationMS); err != nil {
				return err
			}
			if err := writeEdts(b, plan); err != nil {
				return err
			}
			return b.container("mdia", func() error {
				if err := b.leaf("mdhd", &mp4.Mdhd{
					Timescale:  plan.timescale,
					DurationV0: uint32(int64(plan.count) * int64(plan.delta)),
					Language:   [3]byte{'u', 'n', 'd'},
				}, 0); err != nil {
					return err
				}
				if err := b.leaf("hdlr", &mp4.Hdlr{
					HandlerType: handlerType(plan.kind),
					Name:        handlerName(plan.kind),
				}, 0); err != nil {
					return err
				}
				return b.container("minf", func() error {
					if plan.kind == "audio" {
						if err := b.leaf("smhd", &mp4.Smhd{}, 0); err != nil {
							return err
						}
					} else {
						if err := b.leaf("vmhd", &mp4.Vmhd{}, 0); err != nil {
							return err
						}
					}
					return b.container("stbl", func() error {
						if err := writeStsd(b, plan); err != nil {
							return err
						}
						return writeSampleTables(b, plan, sizes, &stcoEntry)
					})
				})
			})
		})
	})
	if err != nil {
		return 0, err
	}
	return stcoEntry, nil
}

func writeTkhd(b *boxWriter, plan trackPlan, durationMS uint32) error {
	t := &mp4.Tkhd{TrackID: plan.trackID, DurationV0: durationMS}
	if plan.kind == "audio" {
		t.Volume = 0x0100
	} else {
		t.Width = 320 << 16
		t.Height = 240 << 16
	}
	return b.leaf("tkhd", t, 0)
}

func writeEdts(b *boxWriter, plan trackPlan) error {
	if len(plan.emptyEdits) == 0 {
		return nil
	}
	return b.container("edts", func() error {
		return b.leaf("elst", &mp4.Elst{
			EntryCount: uint32(len(plan.emptyEdits)),
			Entries:    plan.emptyEdits,
		}, 0)
	})
}

func writeStsd(b *boxWriter, plan trackPlan) error {
	if err := b.open("stsd"); err != nil {
		return err
	}
	if err := b.put(&mp4.Stsd{EntryCount: 1}); err != nil {
		return err
	}
	if plan.kind == "audio" {
		if err := b.open("mp4a"); err != nil {
			return err
		}
		entry := &mp4.AudioSampleEntry{ChannelCount: 2, SampleSize: 16, SampleRate: audioTimescale << 16}
		entry.Type = mp4.BoxTypeMp4a()
		entry.DataReferenceIndex = 1
		if err := b.put(entry); err != nil {
			return err
		}
		if err := b.container("esds", func() error {
			return b.put(&mp4.Esds{Descriptors: []mp4.Descriptor{{
				Tag:                     0x03,
				ESDescriptor:            &mp4.ESDescriptor{ESID: 1},
				DecoderConfigDescriptor: &mp4.DecoderConfigDescriptor{ObjectTypeIndication: 0x40, StreamType: 0x05, MaxBitrate: 128000, AvgBitrate: 128000},
			}}})
		}); err != nil {
			return err
		}
		if err := b.end(); err != nil { // mp4a
			return err
		}
	} else {
		if err := b.open("avc1"); err != nil {
			return err
		}
		entry := &mp4.VisualSampleEntry{
			Width: 320, Height: 240, FrameCount: 1, Depth: 0x18,
			Horizresolution: 0x480000, Vertresolution: 0x480000,
		}
		entry.Type = mp4.StrToBoxType("avc1")
		entry.DataReferenceIndex = 1
		if err := b.put(entry); err != nil {
			return err
		}
		if err := b.open("avcC"); err != nil {
			return err
		}
		cfg := &mp4.AVCDecoderConfiguration{ConfigurationVersion: 1, Profile: 66, Level: 30, LengthSizeMinusOne: 3}
		cfg.Type = mp4.BoxTypeAvcC()
		if err := b.put(cfg); err != nil {
			return err
		}
		if err := b.end(); err != nil { // avcC
			return err
		}
		if err := b.end(); err != nil { // avc1
			return err
		}
	}
	// close stsd
	return b.end()
}

func writeSampleTables(b *boxWriter, plan trackPlan, sizes []uint32, stcoEntry *int64) error {
	if err := b.leaf("stts", &mp4.Stts{
		EntryCount: 1,
		Entries:    []mp4.SttsEntry{{SampleCount: uint32(plan.count), SampleDelta: plan.delta}},
	}, 0); err != nil {
		return err
	}
	if plan.kind == "video" {
		if err := b.leaf("ctts", &mp4.Ctts{
			EntryCount: 1,
			Entries:    []mp4.CttsEntry{{SampleCount: uint32(plan.count), SampleOffsetV0: plan.delta / 2}},
		}, 0); err != nil {
			return err
		}
	}
	if err := b.leaf("stsc", &mp4.Stsc{
		EntryCount: 1,
		Entries:    []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: 1, SampleDescriptionIndex: 1}},
	}, 0); err != nil {
		return err
	}
	if err := b.leaf("stsz", &mp4.Stsz{SampleCount: uint32(plan.count), EntrySize: sizes}, 0); err != nil {
		return err
	}
	bi, err := b.start("stco")
	if err != nil {
		return err
	}
	if err := b.put(&mp4.Stco{EntryCount: uint32(plan.count), ChunkOffset: make([]uint32, plan.count)}); err != nil {
		return err
	}
	*stcoEntry = int64(bi.Offset + bi.HeaderSize + 8) // 4 version/flags + 4 count
	if err := b.end(); err != nil {
		return err
	}
	if plan.kind == "video" {
		keys := make([]uint32, 0, plan.count/plan.keyEvery+1)
		for i := 0; i < plan.count; i++ {
			if plan.keyEvery <= 0 || i%plan.keyEvery == 0 {
				keys = append(keys, uint32(i+1))
			}
		}
		if err := b.leaf("stss", &mp4.Stss{EntryCount: uint32(len(keys)), SampleNumber: keys}, 0); err != nil {
			return err
		}
	}
	return nil
}

// fillPCM writes an interleaved stereo 16-bit sine at 440 Hz with a slow
// amplitude swell, so block loudness is stable across the whole track.
func fillPCM(buf []byte, startFrame int) {
	const frameSamples = 2
	n := len(buf) / 2
	for k := 0; k < n; k++ {
		frame := startFrame + k/frameSamples
		ch := k % frameSamples
		phase := 2 * math.Pi * 440 * float64(frame) / audioTimescale
		if ch == 1 {
			phase += 0.35 // slight decorrelation between channels
		}
		amp := 0.25 + 0.15*math.Sin(2*math.Pi*0.5*float64(frame)/audioTimescale)
		v := int16(math.Round(amp * math.Sin(phase) * 32767))
		binary.LittleEndian.PutUint16(buf[k*2:k*2+2], uint16(v))
	}
}

func writeMdat(f *os.File, payloads [][]byte) error {
	var size uint32 = 8
	for _, p := range payloads {
		size += uint32(len(p))
	}
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], size)
	copy(hdr[4:8], "mdat")
	if _, err := f.Write(hdr[:]); err != nil {
		return err
	}
	for _, p := range payloads {
		if _, err := f.Write(p); err != nil {
			return err
		}
	}
	return nil
}

func patchStco(f *os.File, entryOffset int64, offsets []uint32) error {
	if _, err := f.Seek(entryOffset, io.SeekStart); err != nil {
		return err
	}
	var buf [4]byte
	for _, o := range offsets {
		binary.BigEndian.PutUint32(buf[:], o)
		if _, err := f.Write(buf[:]); err != nil {
			return err
		}
	}
	return nil
}

func handlerType(kind string) [4]byte {
	if kind == "audio" {
		return [4]byte{'s', 'o', 'u', 'n'}
	}
	return [4]byte{'v', 'i', 'd', 'e'}
}

func handlerName(kind string) string {
	if kind == "audio" {
		return "SoundHandler"
	}
	return "VideoHandler"
}

var _ = filepath.Base
