// Package fixture generates deterministic local media, RTP and timeline data.
package fixture

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"

	"github.com/pion/rtp"

	"bcast/internal/model"
	"bcast/internal/render"
	"bcast/internal/rewrap"
	"bcast/internal/safe"
)

// Set contains generated paths and the parsed timeline.
type Set struct {
	Root      *safe.Root
	Timeline  *model.Timeline
	InputRels []string
}

// Generate creates success, corrupt/truncated and boundary fixtures under root.
func Generate(rootPath string, seed int) (*Set, error) {
	root, err := safe.OpenRoot(rootPath)
	if err != nil {
		if err := os.MkdirAll(rootPath, 0o755); err != nil {
			return nil, err
		}
		root, err = safe.OpenRoot(rootPath)
		if err != nil {
			return nil, err
		}
	}
	ctx := context.Background()
	clips := []struct {
		rel string
		id  int
	}{
		{"assets/a.mp4", 0},
		{"assets/b.mp4", 1},
		{"assets/c.mp4", 2},
	}
	for _, c := range clips {
		if err := generateClip(ctx, root, c.rel, seed, c.id, 8); err != nil {
			return nil, err
		}
	}
	if err := generateClip(ctx, root, "assets/boundary-1frame.mp4", seed, 9, 1); err != nil {
		return nil, err
	}
	if err := generateCorrupt(root, seed); err != nil {
		return nil, err
	}
	if err := generateRTP(root); err != nil {
		return nil, err
	}
	tl := timeline(seed)
	b, err := json.MarshalIndent(tl, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := root.WriteFile("timeline/main.json", b, 0o644); err != nil {
		return nil, err
	}
	return &Set{
		Root:     root,
		Timeline: tl,
		InputRels: []string{
			"assets/a.mp4", "assets/b.mp4", "assets/c.mp4",
			"assets/corrupt-truncated.mp4", "records/companion.jsonl",
		},
	}, nil
}

func generateClip(ctx context.Context, root *safe.Root, rel string, seed, clipID, frames int) error {
	ch := make(chan []byte, frames)
	for f := 0; f < frames; f++ {
		ch <- PatternFrame(seed, clipID, f)
	}
	close(ch)
	_, err := rewrap.EncodeRawRGB(ctx, root, rel, render.Width, render.Height, render.FrameRate, ch)
	return err
}

// PatternFrame returns a deterministic seed-derived RGB frame.
func PatternFrame(seed, clipID, frame int) []byte {
	const w, h = render.Width, render.Height
	rgb := make([]byte, w*h*3)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 3
			rgb[i] = byte(seed*7 + clipID*53 + x/2 + frame*11)
			rgb[i+1] = byte(seed*3 + y/2 + frame*7 + clipID*29)
			rgb[i+2] = byte(seed*5 + clipID*91 + (x+y)/5 + frame*3)
		}
	}
	// Moving high-contrast slate block makes frame differences explicit.
	bx := 12 + (frame*9)%96
	by := 18 + clipID*17
	for y := by; y < by+28 && y < h; y++ {
		for x := bx; x < bx+40 && x < w; x++ {
			i := (y*w + x) * 3
			rgb[i], rgb[i+1], rgb[i+2] = 245, 245, 245
		}
	}
	return rgb
}

func generateCorrupt(root *safe.Root, seed int) error {
	rel := "assets/corrupt-truncated.mp4"
	if err := generateClip(context.Background(), root, rel, seed, 7, 8); err != nil {
		return err
	}
	p, err := root.Resolve(rel)
	if err != nil {
		return err
	}
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	keep := st.Size() * 3 / 4
	if keep < 128 {
		keep = st.Size() - 64
	}
	return os.Truncate(p, keep)
}

func generateRTP(root *safe.Root) error {
	type pkt struct {
		seq uint16
		ts  uint32
	}
	pkts := []pkt{
		{4100, 0xfffffff0}, {4101, 0xfffffffa},
		{4101, 0xfffffffa}, {4102, 0x00000004},
	}
	var body []byte
	for i, p := range pkts {
		rtpPkt := rtp.Packet{Header: rtp.Header{
			Version: 2, PayloadType: 96, SequenceNumber: p.seq,
			Timestamp: p.ts, SSRC: 0x53454544, Marker: i == len(pkts)-1,
		}, Payload: []byte{byte('A' + i), 0, 0, 0}}
		raw, err := rtpPkt.Marshal()
		if err != nil {
			return err
		}
		rec := map[string]string{"rtp_b64": base64.StdEncoding.EncodeToString(raw)}
		b, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		body = append(body, b...)
		body = append(body, '\n')
	}
	return root.WriteFile("records/companion.jsonl", body, 0o644)
}

func timeline(seed int) *model.Timeline {
	return &model.Timeline{
		ID:            "nightly-batch",
		FrameDuration: model.Rational{N: 1, D: render.FrameRate},
		Duration:      model.Rational{N: 5, D: 2},
		Tracks: []model.Track{
			{
				ID: "V1", Kind: "video",
				Clips: []model.Clip{
					{ID: "A", Asset: "assets/a.mp4", Start: r0(0), In: r0(0), Out: r1(1)},
					{ID: "B", Asset: "assets/b.mp4", Start: r1(1), In: r0(0), Out: r1(1)},
					{ID: "C", Asset: "assets/c.mp4", Start: r1(2), In: r0(0), Out: model.Rational{N: 1, D: 2}},
				},
			},
			{
				ID: "A1", Kind: "audio", Gain: &model.Ratio{N: 3, D: 4},
				Clips: []model.Clip{{
					ID: "A-AUD", Asset: "assets/a.mp4", Start: r0(0), In: r0(0), Out: r1(1),
					Gain: &model.Ratio{N: 3, D: 4},
				}},
			},
		},
		Transitions: []model.Transition{
			{ID: "DISS-AB", Type: "dissolve", FromClip: "A", ToClip: "B", At: model.Rational{N: 3, D: 4}, Duration: model.Rational{N: 1, D: 4}},
			{ID: "DISS-BC", Type: "dissolve", FromClip: "B", ToClip: "C", At: model.Rational{N: 7, D: 4}, Duration: model.Rational{N: 1, D: 4}},
		},
	}
}

func r0(n int64) model.Rational { return model.Rational{N: n, D: 1} }
func r1(n int64) model.Rational { return model.Rational{N: n, D: 1} }
