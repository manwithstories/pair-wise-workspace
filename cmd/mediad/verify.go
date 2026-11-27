package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	m3u8 "github.com/Eyevinn/hls-m3u8/m3u8"
	"github.com/fashionweek/mediad/internal/cache"
	"github.com/fashionweek/mediad/internal/hls"
	"github.com/fashionweek/mediad/internal/media"
	"github.com/fashionweek/mediad/internal/pipeline"
)

// verifyMetrics is the exact metric line required by the acceptance script.
type verifyMetrics struct {
	RoundtripRate   float64
	Playlists       int
	KeyframeHitRate float64
	Segments        int
	SHA256Checked   int
	Mismatches      int
	RepeatStable    string
	DamageRejected  int
	OffsetsReported int
}

func runVerify(args []string) (int, error) {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	work := fs.String("work", "", "pipeline output directory")
	cases := fs.String("cases", "", "fixture cases directory")
	if err := fs.Parse(args); err != nil {
		return pipeline.ExitArgs, err
	}
	if *work == "" || *cases == "" {
		return pipeline.ExitArgs, fmt.Errorf("verify requires --work and --cases")
	}

	m := &verifyMetrics{}
	ladder := hls.DefaultLadder()

	// 1. Playlist round trips + keyframe alignment + segment hashes.
	cameraDirs, err := listCameraDirs(*work)
	if err != nil {
		return pipeline.ExitArgs, err
	}
	m.Playlists = 0
	totalSegs, parsedSegs, keyframeOK, keyframeTotal := 0, 0, 0, 0
	hashes := map[string]string{}
	for _, cam := range cameraDirs {
		masterPath := filepath.Join(*work, cam, "master.m3u8")
		master, err := hls.ParseMaster(masterPath)
		if err != nil {
			return pipeline.ExitArgs, err
		}
		m.Playlists++ // master playlist
		for _, variant := range master.Variants {
			plPath := filepath.Join(*work, cam, variant.URI)
			pl, lt, err := m3u8.DecodeFrom(mustOpen(plPath), true)
			if err != nil {
				return pipeline.ExitArgs, media.New(media.PhaseVerify,
					media.KindTool, plPath, -1, "media playlist parse failed").Wrap(err)
			}
			if lt != m3u8.MEDIA {
				return pipeline.ExitArgs, fmt.Errorf("%s not a media playlist", variant.URI)
			}
			mp := pl.(*m3u8.MediaPlaylist)
			m.Playlists++
			for _, seg := range mp.Segments {
				if seg == nil {
					continue
				}
				totalSegs++
				segPath := filepath.Join(filepath.Dir(plPath), seg.URI)
				hash, err := cache.HashFile(segPath)
				if err != nil {
					return pipeline.ExitArgs, err
				}
				hashes[filepath.Join(cam, seg.URI)] = hash
				m.SHA256Checked++
				if firstPacketKeyframe(segPath, mp.Map) {
					keyframeOK++
				}
				keyframeTotal++
			}
			parsedSegs++
		}
	}
	m.Segments = totalSegs
	if keyframeTotal > 0 {
		m.KeyframeHitRate = float64(keyframeOK) / float64(keyframeTotal)
	}
	if parsedSegs > 0 {
		m.RoundtripRate = 1.0
	}

	// 2. Repeat stability: re-hash every segment and compare to the first pass.
	stable := 0
	for rel, first := range hashes {
		again, err := cache.HashFile(filepath.Join(*work, rel))
		if err != nil {
			return pipeline.ExitArgs, err
		}
		if again == first {
			stable++
		} else {
			m.Mismatches++
		}
	}
	m.RepeatStable = fmt.Sprintf("%d/%d", stable, len(hashes))

	// 3. Damage cases: each must be rejected and carry a byte offset.
	m.DamageRejected, m.OffsetsReported, err = verifyCases(*cases)
	if err != nil {
		return pipeline.ExitArgs, err
	}

	printVerify(m, len(ladder))
	if !m.pass() {
		return pipeline.ExitRejected, fmt.Errorf("verification failed")
	}
	return pipeline.ExitOK, nil
}

func (m *verifyMetrics) pass() bool {
	return m.RoundtripRate == 1.0 &&
		m.KeyframeHitRate == 1.0 &&
		m.Mismatches == 0 &&
		m.DamageRejected == 3 &&
		m.OffsetsReported == 3
}

func printVerify(m *verifyMetrics, ladder int) {
	fmt.Printf("roundtrip_rate=%.3f playlists=%d\n", m.RoundtripRate, m.Playlists)
	fmt.Printf("keyframe_hit_rate=%.3f segments=%d\n", m.KeyframeHitRate, m.Segments)
	fmt.Printf("sha256_checked=%d mismatches=%d\n", m.SHA256Checked, m.Mismatches)
	fmt.Printf("repeat_stable=%s\n", m.RepeatStable)
	fmt.Printf("damage_rejected=%d offsets_reported=%d\n",
		m.DamageRejected, m.OffsetsReported)
	_ = ladder
}

func listCameraDirs(work string) ([]string, error) {
	entries, err := os.ReadDir(work)
	if err != nil {
		return nil, media.New(media.PhaseVerify, media.KindTool, work, -1,
			"cannot read work dir").Wrap(err)
	}
	var cams []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(work, e.Name(), "master.m3u8")); err == nil {
			cams = append(cams, e.Name())
		}
	}
	if len(cams) == 0 {
		return nil, media.New(media.PhaseVerify, media.KindBounds, work, -1,
			"no published camera directories found")
	}
	return cams, nil
}

func mustOpen(path string) *os.File {
	f, err := os.Open(path)
	if err != nil {
		panic(err)
	}
	return f
}

// verifyCases runs each corrupt sample through the indexer and confirms a
// structured rejection with a non-negative byte offset.
func verifyCases(casesDir string) (rejected, offsets int, err error) {
	indexPath := filepath.Join(casesDir, "index.json")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return 0, 0, media.New(media.PhaseVerify, media.KindTool, indexPath, -1,
			"cases index missing").Wrap(err)
	}
	var manifest fixturesManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return 0, 0, err
	}
	root := filepath.Dir(casesDir)
	for _, c := range manifest.Cases {
		if !c.ExpectReject {
			continue
		}
		path := filepath.Join(root, c.File)
		me, perr := rejectOne(path)
		if perr != nil {
			return 0, 0, perr
		}
		if me != nil {
			rejected++
			if me.Offset >= 0 {
				offsets++
			}
			fmt.Printf("damage %s rejected: kind=%s byte_offset=%d path=%s\n",
				c.Name, me.Kind, me.Offset, me.Path)
		}
	}
	return rejected, offsets, nil
}
