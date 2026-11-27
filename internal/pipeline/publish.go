package pipeline

import (
	"os"
	"path/filepath"
	"sort"

	m3u8 "github.com/Eyevinn/hls-m3u8/m3u8"
	"github.com/fashionweek/mediad/internal/align"
	"github.com/fashionweek/mediad/internal/clock"
	"github.com/fashionweek/mediad/internal/hls"
	"github.com/fashionweek/mediad/internal/media"
)

// publishManifests emits, for every accepted camera, one master playlist and
// one media playlist per ladder variant. Only accepted cameras are published,
// so no playlist can reference a missing segment.
func publishManifests(job *JobConfig, results []*CameraResult,
	tl *align.Timeline, cuts []align.Cut, work string) error {
	ladder := job.LadderOrDefault()
	for _, res := range results {
		camDir := filepath.Join(work, res.Name)
		mediaPL := map[string]*m3u8.MediaPlaylist{}
		ordered := make([]*m3u8.MediaPlaylist, 0, len(ladder))
		for _, v := range ladder {
			spec, err := buildSpec(res.Name, v, cuts)
			if err != nil {
				return err
			}
			pl, err := hls.BuildMediaPlaylist(spec)
			if err != nil {
				return err
			}
			mediaPL[v.Playlist] = pl
			ordered = append(ordered, pl)
		}
		master, err := hls.BuildMasterPlaylist(ladder, ordered)
		if err != nil {
			return err
		}
		if _, err := hls.RenderAll(camDir, "master.m3u8", master, mediaPL); err != nil {
			return err
		}
	}
	return nil
}

// buildSpec turns the planned cuts into a media playlist specification for one
// camera/variant, using the exact rational segment duration for EXTINF.
func buildSpec(camera string, v hls.Variant, cuts []align.Cut) (hls.PlaylistSpec, error) {
	segs := make([]hls.SegmentSpec, len(cuts))
	for i, cut := range cuts {
		dur, err := cut.Till.Sub(cut.At)
		if err != nil {
			return hls.PlaylistSpec{}, media.New(media.PhasePackage,
				media.KindOverflow, "", -1, "segment duration underflow").Wrap(err)
		}
		segs[i] = hls.SegmentSpec{
			Seq:      i,
			URI:      filepath.Join(v.Name, segmentFileName(i)),
			Duration: dur,
		}
	}
	// Variant URIs must point at this variant's init and segment directory.
	variant := v
	variant.InitURI = filepath.Join(v.Name, "init.mp4")
	target := uint(0)
	if len(cuts) > 0 {
		d, _ := cuts[0].Till.Sub(cuts[0].At)
		target = ceilSeconds(d)
	}
	return hls.PlaylistSpec{Variant: variant, Target: target, Segments: segs}, nil
}

func ceilSeconds(r clock.Rat) uint {
	q := r.Num / r.Den
	if r.Num%r.Den != 0 {
		q++
	}
	if q < 1 {
		q = 1
	}
	return uint(q)
}

func segmentFileName(seq int) string {
	return "seg-" + pad3(seq) + ".m4s"
}

func pad3(n int) string {
	if n >= 100 {
		return itoa(n)
	}
	if n >= 10 {
		return "0" + itoa(n)
	}
	return "00" + itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	pos := len(b)
	for n > 0 {
		pos--
		b[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// segmentPath is the canonical on-disk location of one segment.
func segmentPath(work, camera, variant string, seq int) string {
	return filepath.Join(work, camera, variant, segmentFileName(seq))
}

// initPath is the variant init section location.
func initPath(work, camera, variant string) string {
	return filepath.Join(work, camera, variant, "init.mp4")
}

// cameraOutputFiles lists every published artifact for one camera, sorted.
func cameraOutputFiles(work, camera string) []string {
	root := filepath.Join(work, camera)
	var out []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(work, path)
		if err == nil {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}
