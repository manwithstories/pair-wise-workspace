// Fixture preparation for the demo: generated MP4 reels, the material
// manifest, an RTP transport trace, planted cache pollution and the
// interrupted-transfer checkpoint.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"archive.local/archive/internal/cache"
	"archive.local/archive/internal/checkpoint"
	"archive.local/archive/internal/fixture"
	"archive.local/archive/internal/pipeline"
)

func prepareFixtures(work string, seed int64, d time.Duration, corruptAt int64) error {
	secs := int64(d / time.Second)
	if secs <= 0 {
		secs = 1
	}
	// Three long tracks: video 25 fps, audio 48 kHz, and a sparse subtitle
	// track, each starting at a non-zero broadcast origin.
	videoSpan := int64(secs) * 90000 // media span in ticks, independent of origin
	videoSamples := int(videoSpan / (90000 / 25))
	if videoSamples < 800 {
		videoSamples = 800
	}
	// Keep moov compact so mdat begins well before the demo's damage offset
	// (184320 bytes): group samples into chunks and use a constant sample
	// size. That leaves all 90 000 one-hour samples intact while placing
	// their payloads inside the file at byte-accurate offsets.
	videoDelta := uint32(90000 / 25)
	videoOrigin := int64(90000) * secs // one hour in: reels captured mid-broadcast
	// Video access units are represented by a compact proxy payload of 2
	// bytes each: the archive never decodes, and this is what lets a one-hour
	// track fit before the damage byte while retaining all 90 000 sample
	// entries. The free box pads the file so its tail ends just past the
	// requested damage offset, leaving only a handful of samples unreadable.
	video, err := generateAtDamage(filepath.Join(work, "reels", "feature.mp4"), fixture.Clip{
		TrackID: 1, Timescale: 90000, Origin: videoOrigin, Samples: videoSamples,
		Delta: videoDelta, KeyEvery: 50, Kind: "video", Payload: 1,
		ChunkSamples: 50, ConstSize: true,
	}, corruptAt, 400)
	if err != nil {
		return err
	}
	audioSamples := int(secs * 48000 / 1024)
	if audioSamples < 400 {
		audioSamples = 400
	}
	audioOrigin := int64(48000) * secs
	if _, err := fixture.Generate(filepath.Join(work, "reels", "feature.audio.mp4"), fixture.Clip{
		TrackID: 2, Timescale: 48000, Origin: audioOrigin, Samples: audioSamples,
		Delta: 1024, KeyEvery: 1, Kind: "audio", Payload: 96,
	}); err != nil {
		return err
	}
	// A third, sparse data track keeps the multi-track requirement honest.
	dataSamples := int(secs / 2)
	if dataSamples < 100 {
		dataSamples = 100
	}
	if _, err := fixture.Generate(filepath.Join(work, "reels", "feature.slate.mp4"), fixture.Clip{
		TrackID: 3, Timescale: 1000, Origin: int64(1000) * secs, Samples: dataSamples,
		Delta: 2000, KeyEvery: 20, Kind: "video", Payload: 240,
	}); err != nil {
		return err
	}

	// Damage the video master by truncating it at the requested offset, so
	// the sample containing that byte can no longer be read. The reader then
	// reports the first bad sample's exact byte offset, which the demo
	// prints as damage_offset_bytes. A non-positive offset means "healthy
	// master", used to exercise the exit-code-0 path.
	_, damageSample := locateDamage(video, corruptAt)
	if corruptAt > 0 {
		if err := damageTruncate(filepath.Join(work, "reels", "feature.mp4"), corruptAt); err != nil {
			return err
		}
	}
	_ = damageSample

	// Manifest with non-zero origins, edit lists and declared durations.
	var mb strings.Builder
	fmt.Fprintf(&mb, "# long-form material list (generated, seed %d)\n", seed)
	fmt.Fprintf(&mb, "track 1 reels/feature.mp4 90000 %d video %d\n", videoOrigin,
		videoOrigin+int64(videoSamples)*int64(videoDelta))
	fmt.Fprintf(&mb, "track 2 reels/feature.audio.mp4 48000 %d audio %d\n", audioOrigin,
		audioOrigin+int64(audioSamples)*1024)
	fmt.Fprintf(&mb, "track 3 reels/feature.slate.mp4 1000 %d data %d\n", int64(1000)*secs,
		int64(1000)*secs+int64(dataSamples)*2000)
	fmt.Fprintf(&mb, "edit 1 %d %d 1 1\n", videoOrigin, int64(videoSamples)*int64(videoDelta))
	fmt.Fprintf(&mb, "edit 2 -1 48000 1 1\n")
	fmt.Fprintf(&mb, "edit 2 %d %d 1 1\n", audioOrigin, int64(audioSamples)*1024)
	fmt.Fprintf(&mb, "edit 3 %d %d 1 1\n", int64(1000)*secs, int64(dataSamples)*2000)
	if err := os.WriteFile(filepath.Join(work, "material.txt"), []byte(mb.String()), 0o600); err != nil {
		return err
	}

	// Plant a polluted cache entry so the run's verification stage detects a
	// digest mismatch, quarantines it and reprocesses. A stale cache left by
	// an earlier interrupted transfer is the realistic cause.
	if err := plantCachePollution(work); err != nil {
		return err
	}

	// Transport trace with duplicates, reordering, a burst and a wrap.
	// Start near the end of the sequence space so a wrap is observed.
	_, err = fixture.GenerateTrace(filepath.Join(work, "transport.trace"), fixture.TracePlan{
		Packets: max(2400, int(secs)*2), Payload: 512,
		DupEvery: 97, ReorderEvery: 131,
		Bursts:   []fixture.Burst{{At: 600, Count: 12}, {At: 1800, Count: 20}},
		StartSeq: 65300,
	}, seed)
	return err
}

// generateAtDamage writes a clip whose mdat payload ends exactly `tailBytes`
// past damageAt, so a truncation at damageAt leaves only the final few samples
// unreadable. It first generates without padding to learn the exact moov
// layout, then regenerates with the free box that lands the tail precisely.
func generateAtDamage(path string, clip fixture.Clip, damageAt, tailBytes int64) (*fixture.Layout, error) {
	if damageAt <= 0 {
		return fixture.Generate(path, clip)
	}
	probe, err := fixture.Generate(path, clip)
	if err != nil {
		return nil, err
	}
	last := len(probe.Offsets) - 1
	if last < 0 {
		return probe, nil
	}
	payloadEnd := int64(probe.Offsets[last]) + int64(probe.Sizes[last])
	if payloadEnd >= damageAt {
		// The untouched payload already extends past the damage point; leave
		// it unpadded so the truncation still bites.
		return probe, nil
	}
	pad := damageAt + tailBytes - payloadEnd + 8
	clip.PadBeforeMdat = int(pad)
	return fixture.Generate(path, clip)
}

// locateDamage returns the byte offset of the first sample that crosses the
// requested corruption point, plus its index. If the request is past the
// media payload, the last sample is used so the demo still has a defect.
func locateDamage(layout *fixture.Layout, want int64) (int64, int) {
	for i, off := range layout.Offsets {
		if int64(off) >= want {
			return int64(off), i
		}
	}
	last := len(layout.Offsets) - 1
	if last < 0 {
		return want, 0
	}
	return int64(layout.Offsets[last]), last
}

// damageTruncate cuts the master exactly at offset, simulating a transfer
// that lost its tail. The sample that owned that byte becomes unreadable and
// is reported at that precise offset.
func damageTruncate(path string, offset int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if offset <= 0 || offset >= info.Size() {
		return fmt.Errorf("damage offset %d outside file of %d bytes", offset, info.Size())
	}
	return os.Truncate(path, offset)
}

func fixtureSetPresent(work string) bool {
	for _, name := range []string{"material.txt", "reels/feature.mp4",
		"reels/feature.audio.mp4", "reels/feature.slate.mp4", "transport.trace"} {
		if _, err := os.Stat(filepath.Join(work, name)); err != nil {
			return false
		}
	}
	return true
}

func checkpointExists(work string) bool {
	_, err := os.Stat(filepath.Join(work, "state", "checkpoint.json"))
	return err == nil
}

// plantCachePollution seeds the cache with a single corrupted object whose
// metadata claims a valid digest. The next pipeline pass reads it, finds the
// mismatch and quarantines it.
func plantCachePollution(work string) error {
	store, err := cache.NewStore(filepath.Join(work, "cache"))
	if err != nil {
		return err
	}
	good := []byte("interrupted-transfer-cache-entry")
	src := cache.Provenance(strings.Repeat("0", 64), 99, 0, 0, uint64(len(good)))
	e, err := store.Put(good, src)
	if err != nil {
		return err
	}
	_, err = store.Poison(e.Address)
	return err
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// simulateInterrupt persists a checkpoint describing an interrupted transfer:
// the manifest is parsed and indexed exactly as the real run will, then a
// partial block set and the packet count confirmed so far are recorded. The
// subsequent --resume run must re-validate all of it.
func simulateInterrupt(work string, o pipeline.Options, packets int, seed int64) error {
	if packets <= 0 {
		packets = 1200
	}
	st, err := pipeline.CheckpointFor(work, o.ManifestPath, packets)
	if err != nil {
		return err
	}
	ck, err := checkpoint.NewStore(filepath.Join(work, "state"))
	if err != nil {
		return err
	}
	return ck.Save(st)
}

func min(a, b int) int {

	if a < b {
		return a
	}
	return b
}
