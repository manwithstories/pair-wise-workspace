package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"segctl/internal/ingest"
	"segctl/internal/m3u8"
	"segctl/internal/verify"
)

func runManifest(args []string) (int, error) {
	fs := flag.NewFlagSet("manifest", flag.ContinueOnError)
	root := fs.String("root", "testdata/slices", "sliced output root with <rendition>/meta.json")
	masterPath := fs.String("master", "testdata/index.m3u8", "master playlist output path")
	allowGap := fs.Bool("allow-gap", false, "publish playlists containing EXT-X-GAP entries")
	strict := fs.Bool("strict", false, "fail the run when any gap is present")
	if err := fs.Parse(args); err != nil {
		return 1, err
	}

	entries, err := os.ReadDir(*root)
	if err != nil {
		return 1, fmt.Errorf("manifest: read %s: %w", *root, err)
	}
	var metas []*ingest.RenditionMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := verify.CheckRendition(e.Name()); err != nil {
			return 1, err
		}
		m, err := ingest.ReadMeta(filepath.Join(*root, e.Name(), "meta.json"))
		if err != nil {
			continue // not a sliced rendition directory
		}
		metas = append(metas, m)
	}
	if len(metas) == 0 {
		return 1, fmt.Errorf("manifest: no rendition metadata under %s", *root)
	}
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].TargetBitrateKbps > metas[j].TargetBitrateKbps
	})

	totalGaps := 0
	master := &m3u8.Master{Version: 7}
	playlists := map[string]*m3u8.Media{}
	for _, m := range metas {
		media, gaps, err := buildMedia(m)
		if err != nil {
			return 1, fmt.Errorf("manifest: %s: %w", m.Rendition, err)
		}
		if err := media.Validate(); err != nil {
			return 1, fmt.Errorf("manifest: %s: %w", m.Rendition, err)
		}
		totalGaps += gaps
		playlists[m.Rendition] = media
		master.Variants = append(master.Variants, m3u8.Variant{
			Name:      m.Rendition,
			Bandwidth: m.TargetBitrateKbps * 1000,
			Width:     m.Width,
			Height:    m.Height,
			URI:       m.Rendition + "/index.m3u8",
		})
	}
	if totalGaps > 0 && *strict && !*allowGap {
		return 3, fmt.Errorf("manifest: %d gap(s) present and --strict is set", totalGaps)
	}
	for _, m := range metas {
		plPath := filepath.Join(*root, m.Rendition, "index.m3u8")
		if err := writeFileAtomic(plPath, []byte(playlists[m.Rendition].Render())); err != nil {
			return 1, err
		}
	}
	if err := writeFileAtomic(*masterPath, []byte(master.Render())); err != nil {
		return 1, err
	}
	fmt.Printf("manifest renditions=%d gaps=%d master=%s\n", len(metas), totalGaps, *masterPath)
	return 0, nil
}

// buildMedia expands segments and gap placeholders into a media playlist.
// Gap entries occupy their sequence slot but carry no real duration weight
// in the verification accounting.
func buildMedia(m *ingest.RenditionMeta) (*m3u8.Media, int, error) {
	bySeq := map[int]ingest.SegmentMeta{}
	maxSeq := -1
	maxDur := 0
	for _, s := range m.Segments {
		bySeq[s.Seq] = s
		if s.Seq > maxSeq {
			maxSeq = s.Seq
		}
		if s.DurationMs > maxDur {
			maxDur = s.DurationMs
		}
	}
	gapAt := map[int]int{}
	for _, g := range m.Gaps {
		gapAt[g.Seq] = g.Count
		if g.Seq+g.Count-1 > maxSeq {
			maxSeq = g.Seq + g.Count - 1
		}
	}
	target := (maxDur + 999) / 1000
	if target < 1 {
		target = 1
	}
	media := &m3u8.Media{
		Version:        7,
		TargetDuration: target,
		MediaSequence:  0,
		PlaylistType:   "VOD",
		EndList:        true,
	}
	gapCount := 0
	for seq := 0; seq <= maxSeq; seq++ {
		if n, ok := gapAt[seq]; ok {
			for k := 0; k < n; k++ {
				media.Segments = append(media.Segments, m3u8.Segment{
					DurationMs: m.TargetMs,
					URI:        fmt.Sprintf("gap_%06d.ts", seq+k),
					Gap:        true,
				})
				gapCount++
			}
			seq += n - 1
			continue
		}
		s, ok := bySeq[seq]
		if !ok {
			return nil, 0, fmt.Errorf("sequence %d is neither segment nor gap", seq)
		}
		crc, err := strconv.ParseUint(s.CRC32, 16, 32)
		if err != nil {
			return nil, 0, fmt.Errorf("segment %d: bad crc32 %q", seq, s.CRC32)
		}
		media.Segments = append(media.Segments, m3u8.Segment{
			DurationMs: s.DurationMs,
			URI:        s.File,
			CRC:        uint32(crc),
			HasCRC:     true,
		})
	}
	return media, gapCount, nil
}
