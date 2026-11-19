package testdata

import (
	"math/rand"
	"os"
	"path/filepath"
)

func readFile(dir, name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(dir, name))
}

// simplePlaylist writes a 3-entry HLS media playlist as entry manifest.
func simplePlaylist(entries []string) string {
	s := "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:1\n"
	for _, e := range entries {
		s += "#EXTINF:1.0,\n" + e + "\n"
	}
	return s + "#EXT-X-ENDLIST\n"
}

func oneSegment(rng *rand.Rand, i int) []byte {
	return buildSegment(rng, stdTracks(), []int64{int64(i) * 90000, audioBase(i), int64(i) * 1000}, false, 24000)
}

// genTruncated: the third segment is cut mid-box (single-track truncation
// surfaces as a demux error with a byte offset).
func genTruncated(rng *rand.Rand, dir string) (string, error) {
	files := []string{"seg_00001.m4s", "seg_00002.m4s", "seg_00003.m4s"}
	for i, name := range files {
		data := oneSegment(rng, i)
		if i == 2 {
			data = data[:len(data)*3/5] // truncated
		}
		if err := writeFile(dir, name, data); err != nil {
			return "", err
		}
	}
	return "media.m3u8", writeFile(dir, "media.m3u8", []byte(simplePlaylist(files)))
}

// genEmpty: the second segment is a zero-byte file.
func genEmpty(rng *rand.Rand, dir string) (string, error) {
	files := []string{"seg_00001.m4s", "seg_00002.m4s"}
	for i, name := range files {
		var data []byte
		if i == 0 {
			data = oneSegment(rng, i)
		}
		if err := writeFile(dir, name, data); err != nil {
			return "", err
		}
	}
	return "media.m3u8", writeFile(dir, "media.m3u8", []byte(simplePlaylist(files)))
}

// genMissingSeg: the manifest references a segment that does not exist.
func genMissingSeg(rng *rand.Rand, dir string) (string, error) {
	files := []string{"seg_00001.m4s", "seg_00002.m4s", "seg_00009.m4s"}
	for i := 0; i < 2; i++ {
		if err := writeFile(dir, files[i], oneSegment(rng, i)); err != nil {
			return "", err
		}
	}
	return "media.m3u8", writeFile(dir, "media.m3u8", []byte(simplePlaylist(files)))
}

// genClockOverflow: the second segment's decode time jumps backwards far
// beyond any wrap-around explanation; remux must reject the track.
func genClockOverflow(rng *rand.Rand, dir string) (string, error) {
	st := stdTracks()
	seg0 := buildSegment(rng, st, []int64{180000, 0, 0}, false, 24000)
	seg1 := buildSegment(rng, st, []int64{90000, 48128, 1000}, false, 24000) // DTS 90000 < 180000
	if err := writeFile(dir, "seg_00001.m4s", seg0); err != nil {
		return "", err
	}
	if err := writeFile(dir, "seg_00002.m4s", seg1); err != nil {
		return "", err
	}
	files := []string{"seg_00001.m4s", "seg_00002.m4s"}
	return "media.m3u8", writeFile(dir, "media.m3u8", []byte(simplePlaylist(files)))
}

// RTPStream synthesizes a transmitted RTP stream: n packets from base
// sequence number, with one burst of burstLoss dropped packets, windowed
// reordering of up to reorder positions, and ~2% duplicates.
func RTPStream(rng *rand.Rand, base uint16, n, burstLoss, reorder int) [][]byte {
	burstStart := n / 3
	var seqs []uint16
	for i := 0; i < n; i++ {
		if i >= burstStart && i < burstStart+burstLoss {
			continue // burst loss
		}
		seqs = append(seqs, base+uint16(i))
	}
	// windowed reordering: shuffle within windows of `reorder` packets
	if reorder > 1 {
		for start := 0; start < len(seqs); start += reorder {
			end := start + reorder
			if end > len(seqs) {
				end = len(seqs)
			}
			rng.Shuffle(end-start, func(a, b int) {
				seqs[start+a], seqs[start+b] = seqs[start+b], seqs[start+a]
			})
		}
	}
	// ~2% duplicates of a recent packet
	var tx [][]byte
	for i, s := range seqs {
		tx = append(tx, marshalRTP(s, uint32(0xFFFF0000)+uint32(s-base)*3000))
		if i > 4 && rng.Intn(50) == 0 {
			tx = append(tx, marshalRTP(seqs[i-2], uint32(0xFFFF0000)+uint32(seqs[i-2]-base)*3000))
		}
	}
	return tx
}

func marshalRTP(seq uint16, ts uint32) []byte {
	// minimal RTP marshaler (V=2, PT=96, SSRC=42); kept local so fixture
	// generation does not depend on recovery internals
	payload := make([]byte, 188)
	for i := range payload {
		payload[i] = byte(seq) ^ byte(i)
	}
	b := make([]byte, 12+len(payload))
	b[0] = 0x80 // V=2
	b[1] = 96   // PT
	b[2] = byte(seq >> 8)
	b[3] = byte(seq)
	b[4] = byte(ts >> 24)
	b[5] = byte(ts >> 16)
	b[6] = byte(ts >> 8)
	b[7] = byte(ts)
	b[8], b[9], b[10], b[11] = 0, 0, 0, 42 // SSRC
	copy(b[12:], payload)
	return b
}
