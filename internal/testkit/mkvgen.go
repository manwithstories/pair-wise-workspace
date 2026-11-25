package testkit

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"sort"

	"mediapipe/internal/ebml"
	"mediapipe/internal/media"
)

// Build encodes spec into a complete Matroska/WebM byte stream.
func Build(spec Spec) SampleFile {
	samples := SamplesFor(spec)
	raw := encodeFile(spec, samples)
	res := SampleFile{Spec: spec, Samples: samples}
	for _, s := range samples {
		if s.LateBy != 0 {
			res.Late = append(res.Late, s)
		}
	}
	if spec.TruncateAt > 0 && spec.TruncateAt < int64(len(raw)) {
		raw = raw[:spec.TruncateAt]
	}
	res.Bytes = raw
	return res
}

// Matroska tick is one millisecond.
const tick = int64(media.Millisecond)

func encodeFile(spec Spec, samples []SyncSample) []byte {
	var out bytes.Buffer
	writeEBMLHeader(&out, spec.DocType)
	writeSegment(&out, spec, samples)
	return out.Bytes()
}

func writeEBMLHeader(buf *bytes.Buffer, docType string) {
	var payload bytes.Buffer
	w := ebml.NewWriter(&payload, nil)
	w.Uint(ebml.IDEBMLVersion, 1)
	w.Uint(ebml.IDEBMLReadVersion, 1)
	w.Uint(ebml.IDEBMLMaxIDLength, 4)
	w.Uint(ebml.IDEBMLMaxSizeLength, 8)
	w.String(ebml.IDDocType, docType)
	w.Uint(ebml.IDDocTypeVersion, 4)
	payload.Write([]byte{0xEC, 0x80}) // Void, empty, keeps offsets interesting
	hdr := ebml.NewWriter(buf, nil)
	hdr.Element(ebml.IDEBML, payload.Bytes())
}

// cluster groups samples for one span.
type clusterGroup struct {
	start int64
	items []SyncSample
}

func writeSegment(buf *bytes.Buffer, spec Spec, samples []SyncSample) {
	var seg bytes.Buffer
	w := ebml.NewWriter(&seg, nil)
	writeInfo(w, spec)
	writeTracks(w, spec)
	for _, g := range groupClusters(spec, samples) {
		writeCluster(w, spec, g)
	}
	if spec.UnknownSegment {
		w := ebml.NewWriter(buf, nil)
		w.WriteID(ebml.IDSegment)
		w.WriteUnknownSize()
		buf.Write(seg.Bytes())
		return
	}
	hdr := ebml.NewWriter(buf, nil)
	hdr.Element(ebml.IDSegment, seg.Bytes())
}

func writeInfo(w *ebml.Writer, spec Spec) {
	var info bytes.Buffer
	iw := ebml.NewWriter(&info, nil)
	iw.Uint(ebml.IDTimestampScale, uint64(tick))
	iw.Float(ebml.IDDuration, float64(spec.Duration)/float64(tick), 8)
	iw.String(ebml.IDMuxingApp, "mediapipe-synth")
	iw.String(ebml.IDWritingApp, "mediapipe-synth")
	w.Element(ebml.IDInfo, info.Bytes())
}

func writeTracks(w *ebml.Writer, spec Spec) {
	var tracks bytes.Buffer
	tw := ebml.NewWriter(&tracks, nil)
	for _, t := range spec.Tracks {
		var entry bytes.Buffer
		ew := ebml.NewWriter(&entry, nil)
		ew.Uint(ebml.IDTrackNumber, t.ID)
		ew.Uint(ebml.IDTrackUID, t.UID)
		ew.Uint(ebml.IDTrackType, uint64(t.Type))
		ew.Uint(ebml.IDFlagLacing, 1)
		if t.DefaultDuration > 0 {
			ew.Uint(ebml.IDDefaultDuration, uint64(t.DefaultDuration))
		}
		ew.String(ebml.IDLanguage, t.Language)
		ew.String(ebml.IDCodecID, string(t.Codec))
		if len(t.CodecPrivate) > 0 {
			ew.Bytes(ebml.IDCodecPrivate, t.CodecPrivate)
		}
		switch t.Type {
		case media.TrackVideo:
			var video bytes.Buffer
			vw := ebml.NewWriter(&video, nil)
			vw.Uint(ebml.IDPixelWidth, uint64(t.Width))
			vw.Uint(ebml.IDPixelHeight, uint64(t.Height))
			ew.Element(ebml.IDVideo, video.Bytes())
		case media.TrackAudio:
			var audio bytes.Buffer
			aw := ebml.NewWriter(&audio, nil)
			aw.Uint(ebml.IDChannels, uint64(t.Channels))
			aw.Float(ebml.IDSamplingFrequency, float64(t.SampleRate), 4)
			ew.Element(ebml.IDAudio, audio.Bytes())
		}
		tw.Element(ebml.IDTrackEntry, entry.Bytes())
	}
	w.Element(ebml.IDTracks, tracks.Bytes())
}

func groupClusters(spec Spec, samples []SyncSample) []clusterGroup {
	span := spec.ClusterSpan
	if span <= 0 {
		span = 2 * media.Second
	}
	var groups []clusterGroup
	index := make(map[int64]int)
	for _, s := range samples {
		key := int64(s.DTS) / int64(span)
		start := media.Timestamp(key * int64(span))
		i, ok := index[int64(start)]
		if !ok {
			i = len(groups)
			index[int64(start)] = i
			groups = append(groups, clusterGroup{start: int64(start)})
		}
		groups[i].items = append(groups[i].items, s)
	}
	for _, g := range groups {
		sort.SliceStable(g.items, func(i, j int) bool {
			if g.items[i].DTS != g.items[j].DTS {
				return g.items[i].DTS < g.items[j].DTS
			}
			return g.items[i].Track < g.items[j].Track
		})
	}
	return groups
}

func writeCluster(w *ebml.Writer, spec Spec, g clusterGroup) {
	var body bytes.Buffer
	bw := ebml.NewWriter(&body, nil)
	bw.Uint(ebml.IDTimestamp, uint64(g.start/tick))

	// Frame order follows the source order; LateBy reorders write position
	// while keeping each frame's own DTS, which is what the late-arrival
	// fixture needs.
	items := append([]SyncSample(nil), g.items...)
	if spec.LateFrames > 0 {
		sort.SliceStable(items, func(i, j int) bool {
			ai := items[i].DTS - items[i].LateBy
			aj := items[j].DTS - items[j].LateBy
			if ai != aj {
				return ai < aj
			}
			if items[i].DTS != items[j].DTS {
				return items[i].DTS < items[j].DTS
			}
			return items[i].Track < items[j].Track
		})
	}

	type block struct {
		first media.Timestamp
		items []SyncSample
	}
	var blocks []block
	i := 0
	for i < len(items) {
		cur := items[i]
		group := 1
		if spec.Lacing != 0 && spec.LaceFrames > 1 && cur.Track != 1 &&
			sameLaceGroup(items[i:], spec.LaceFrames, cur.Track) {
			group = spec.LaceFrames
		}
		blocks = append(blocks, block{first: cur.DTS, items: items[i : i+group]})
		i += group
	}
	// Emit blocks in effective arrival order, stable on DTS then track.
	sort.SliceStable(blocks, func(i, j int) bool {
		if blocks[i].first != blocks[j].first {
			return blocks[i].first < blocks[j].first
		}
		return blocks[i].items[0].Track < blocks[j].items[0].Track
	})
	for _, b := range blocks {
		laced := len(b.items) > 1
		_ = writeBlock(bw, spec, g.start, b.items, spec.Lacing, laced)
	}

	if spec.ClusterCRC {
		payload := body.Bytes()
		crc := crc32.ChecksumIEEE(payload)
		var withCRC bytes.Buffer
		cw := ebml.NewWriter(&withCRC, nil)
		cw.CRC32Element(crc)
		withCRC.Write(payload)
		emitCluster(w, spec, withCRC.Bytes())
		return
	}
	emitCluster(w, spec, body.Bytes())
}

func emitCluster(w *ebml.Writer, spec Spec, payload []byte) {
	if spec.UnknownCluster {
		w.WriteID(ebml.IDCluster)
		w.WriteUnknownSize()
		w.WriteRaw(payload)
		return
	}
	w.Element(ebml.IDCluster, payload)
}

func sameLaceGroup(items []SyncSample, n int, track uint64) bool {
	if len(items) < n {
		return false
	}
	for i := 0; i < n; i++ {
		if items[i].Track != track {
			return false
		}
	}
	return true
}

func writeBlock(w *ebml.Writer, spec Spec, clusterTS int64, items []SyncSample, lacing byte, laced bool) error {
	var payload bytes.Buffer
	encodeVint(&payload, items[0].Track)
	rel := int16((int64(items[0].DTS) - clusterTS) / tick)
	var relBuf [2]byte
	binary.BigEndian.PutUint16(relBuf[:], uint16(rel))
	payload.Write(relBuf[:])
	var flags byte
	if items[0].Keyframe {
		flags |= 0x80
	}
	if laced && lacing != 0 {
		flags |= lacing << 1
		payload.WriteByte(flags)
		payload.WriteByte(byte(len(items) - 1))
		switch lacing {
		case 1: // xiph
			for i := 0; i < len(items)-1; i++ {
				n := len(items[i].Data)
				for n >= 255 {
					payload.WriteByte(255)
					n -= 255
				}
				payload.WriteByte(byte(n))
			}
		case 2: // fixed: nothing to write
		case 3: // ebml
			for i := 0; i < len(items)-1; i++ {
				encodeVint(&payload, uint64(len(items[i].Data)))
			}
		}
	} else {
		payload.WriteByte(flags)
	}
	for _, it := range items {
		payload.Write(it.Data)
	}
	return w.Element(ebml.IDSimpleBlock, payload.Bytes())
}

func encodeVint(buf *bytes.Buffer, v uint64) {
	b, err := ebml.EncodeSize(int64(v))
	if err != nil {
		buf.WriteByte(0xFF)
		return
	}
	buf.Write(b)
}
