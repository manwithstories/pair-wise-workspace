// Package fixture builds deterministic, self-contained media trees for the
// demo and tests. Everything is produced from a seed; no network or external
// binaries are involved.
package fixture

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mediad/internal/media"
)

// Camera describes one camera feed in a generated tree.
type Camera struct {
	Name      string `json:"name"`
	Language  string `json:"language"`
	Role      string `json:"role"`
	Codec     string `json:"codec"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Timescale int64  `json:"timescale"`
}

// Profile selects the fixture shape.
type Profile string

const (
	ProfileClean   Profile = "clean"
	ProfileRecover Profile = "recover"
	ProfileCorrupt Profile = "corrupt"
)

// Options configures generation.
type Options struct {
	Seed          int64
	Profile       Profile
	Cams          int
	Segments      int
	Root          string
	Loss          float64
	DriftPPM      float64
	WrapPTS       int64
	VFR           bool
	TruncateMoof  int64
	Container     string // "cmaf" (TS fixtures are built in tests)
	FragmentDur   int64  // ticks per segment on the 90 kHz axis
	FramesPerFrag int
}

// Info is the manifest of a generated tree, read by the demo runner.
type Info struct {
	Seed          int64    `json:"seed"`
	Profile       Profile  `json:"profile"`
	Cams          int      `json:"cams"`
	Segments      int      `json:"segments"`
	Container     string   `json:"container"`
	FragmentDur   int64    `json:"fragment_dur"`
	FramesPerFrag int      `json:"frames_per_frag"`
	WrapPTS       int64    `json:"wrap_pts"`
	VFR           bool     `json:"vfr"`
	Loss          float64  `json:"loss"`
	DriftPPM      float64  `json:"drift_ppm"`
	CameraList    []Camera `json:"cameras"`
	Expected      Expected `json:"expected"`
}

// Expected is generator ground truth used by independent checks.
type Expected struct {
	SamplesPerCam    map[string]int `json:"samples_per_cam"`
	TotalSamples     int            `json:"total_samples"`
	PayloadSHA       string         `json:"payload_sha"` // order-independent digest
	LostPackets      int            `json:"lost_packets"`
	RecoveredPackets int            `json:"recovered_packets"`
	RTPFrames        int            `json:"rtp_frames"`
	TruncatedSegs    int            `json:"truncated_segments"`
}

// Defaults fills unset option fields.
func (o *Options) Defaults() {
	if o.Cams <= 0 {
		o.Cams = 4
	}
	if o.Segments <= 0 {
		o.Segments = 180
	}
	if o.Profile == "" {
		o.Profile = ProfileClean
	}
	if o.Container == "" {
		o.Container = "cmaf"
	}
	if o.FragmentDur <= 0 {
		o.FragmentDur = 12000 // 4 frames at 30 fps
	}
	if o.FramesPerFrag <= 0 {
		o.FramesPerFrag = int(o.FragmentDur / 3000)
		if o.FramesPerFrag < 1 {
			o.FramesPerFrag = 1
		}
	}
}

// DefaultCameras assigns stable roles and codecs per camera.
func DefaultCameras(n int) []Camera {
	roles := []string{"main", "side", "roaming", "main", "side", "roaming"}
	out := make([]Camera, n)
	for i := 0; i < n; i++ {
		role := roles[i%len(roles)]
		codec := "avc1"
		if role == "roaming" {
			codec = "hvc1"
		}
		out[i] = Camera{
			Name:      fmt.Sprintf("cam%d", i+1),
			Language:  "und",
			Role:      role,
			Codec:     codec,
			Width:     1920 - i*128,
			Height:    1080 - i*72,
			Timescale: 90000,
		}
	}
	return out
}

// DriftForCamera returns the per-camera clock skew in ppm. Camera 1 is the
// reference; later cameras drift progressively so cross-camera correction is
// observable.
func DriftForCamera(basePPM float64, idx int, cams int) float64 {
	if basePPM == 0 {
		return 0
	}
	steps := cams - 1
	if steps < 1 {
		steps = 1
	}
	return basePPM * float64(idx) / float64(steps)
}

// Generate writes a complete fixture tree under opts.Root and returns Info.
func Generate(opts Options) (*Info, error) {
	opts.Defaults()
	if opts.Root == "" {
		return nil, fmt.Errorf("fixture: root required")
	}
	cams := DefaultCameras(opts.Cams)
	info := &Info{
		Seed:          opts.Seed,
		Profile:       opts.Profile,
		Cams:          opts.Cams,
		Segments:      opts.Segments,
		Container:     opts.Container,
		FragmentDur:   opts.FragmentDur,
		FramesPerFrag: opts.FramesPerFrag,
		WrapPTS:       opts.WrapPTS,
		VFR:           opts.VFR,
		Loss:          opts.Loss,
		DriftPPM:      opts.DriftPPM,
		CameraList:    cams,
		Expected: Expected{
			SamplesPerCam: make(map[string]int, len(cams)),
		},
	}

	mediaDir := filepath.Join(opts.Root, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		return nil, err
	}

	truncateAt := opts.Segments / 2
	digests := make([]string, 0, len(cams)*opts.Segments*opts.FramesPerFrag)
	var totalSamples int
	for ci, cam := range cams {
		rng := rand.New(rand.NewSource(opts.Seed*1000 + int64(ci)))
		st := &camState{
			cam:      cam,
			rng:      rng,
			driftPPM: DriftForCamera(opts.DriftPPM, ci, len(cams)),
			vfr:      opts.VFR,
			wrapBase: opts.WrapPTS,
			fragDur:  opts.FragmentDur,
			frames:   opts.FramesPerFrag,
		}
		if err := os.MkdirAll(filepath.Join(mediaDir, cam.Name), 0o755); err != nil {
			return nil, err
		}
		for si := 0; si < opts.Segments; si++ {
			seg, payloads, err := st.buildSegment(si)
			if err != nil {
				return nil, err
			}
			if opts.Profile == ProfileCorrupt && ci == 0 && si == truncateAt {
				seg = truncateMoof(seg, opts.TruncateMoof)
				info.Expected.TruncatedSegs = 1
			}
			rel := cam.Name + "/" + fmt.Sprintf("seg%d.m4s", si)
			if err := os.WriteFile(filepath.Join(mediaDir, filepath.FromSlash(rel)), seg, 0o644); err != nil {
				return nil, err
			}
			for _, p := range payloads {
				digests = append(digests, fmt.Sprintf("%x", sha256.Sum256(p)))
				totalSamples++
			}
			info.Expected.SamplesPerCam[cam.Name] += len(payloads)
		}
		initSeg := buildInitSegment(cam, opts.FragmentDur)
		if err := os.WriteFile(filepath.Join(mediaDir, cam.Name, "init.mp4"), initSeg, 0o644); err != nil {
			return nil, err
		}
	}
	info.Expected.TotalSamples = totalSamples
	sort.Strings(digests)
	h := sha256.New()
	for _, d := range digests {
		h.Write([]byte(d))
	}
	info.Expected.PayloadSHA = fmt.Sprintf("%x", h.Sum(nil))

	if err := writeManifests(opts, cams, mediaDir); err != nil {
		return nil, err
	}
	if opts.Profile == ProfileRecover {
		if err := writeRTPFixture(opts, cams, info, mediaDir); err != nil {
			return nil, err
		}
	}
	if err := writeInfo(opts.Root, info); err != nil {
		return nil, err
	}
	return info, nil
}

// camState generates one camera's segments. Local timestamps apply this
// camera's clock drift; the shared reference axis remains the manifest timing.
type camState struct {
	cam      Camera
	rng      *rand.Rand
	driftPPM float64
	vfr      bool
	wrapBase int64
	fragDur  int64
	frames   int
}

// localTicks maps a reference tick to this camera's local clock. A wrapBase
// shift makes the local clock cross the 33-bit boundary mid-stream, so its
// on-wire PTS/DTS values wrap exactly as a real MPEG clock would.
func (st *camState) localTicks(ref int64) int64 {
	drift := int64(float64(ref) * st.driftPPM / 1e6)
	return st.wrapBase + ref + drift
}

// buildSegment returns the segment bytes and the ordered sample payloads.
func (st *camState) buildSegment(index int) ([]byte, [][]byte, error) {
	refStart := int64(index) * st.fragDur
	payloads := make([][]byte, st.frames)
	durs := make([]int64, st.frames)
	for f := 0; f < st.frames; f++ {
		dur := int64(3000)
		if st.vfr {
			// Deterministic VFR jitter, still positive frame duration.
			dur += int64(st.rng.Intn(1201)) - 600
		}
		durs[f] = dur
		payloads[f] = samplePayload(st.cam, index, f)
	}
	localStart := st.localTicks(refStart)
	// tfdt carries the low 33 bits; the adapter unwraps against its running
	// reference so a mid-stream 2^33 crossing is reconstructed exactly.
	encoded := ((localStart % media.Wrap33) + media.Wrap33) % media.Wrap33
	seg := buildFragmentedSegment(st.cam, index, encoded, durs, payloads)
	return seg, payloads, nil
}

func samplePayload(cam Camera, seg, frame int) []byte {
	h := sha256.New()
	fmt.Fprintf(h, "%s/%d/%d", cam.Name, seg, frame)
	sum := h.Sum(nil)
	out := make([]byte, 256)
	for i := range out {
		out[i] = sum[i%len(sum)] ^ byte(i*7+seg)
	}
	return out
}

func writeInfo(root string, info *Info) error {
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "fixture.json"), data, 0o644)
}

// ReadInfo loads a generated fixture description.
func ReadInfo(root string) (*Info, error) {
	data, err := os.ReadFile(filepath.Join(root, "fixture.json"))
	if err != nil {
		return nil, err
	}
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func writeManifests(opts Options, cams []Camera, mediaDir string) error {
	segDur := float64(opts.FragmentDur) / 90000.0
	var master strings.Builder
	master.WriteString("#EXTM3U\n#EXT-X-VERSION:6\n")
	for _, cam := range cams {
		var pl strings.Builder
		pl.WriteString("#EXTM3U\n#EXT-X-VERSION:6\n")
		fmt.Fprintf(&pl, "#EXT-X-TARGETDURATION:%d\n", int(segDur+0.999))
		pl.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
		for si := 0; si < opts.Segments; si++ {
			fmt.Fprintf(&pl, "#EXTINF:%.6f,\nseg%d.m4s\n", segDur, si)
		}
		pl.WriteString("#EXT-X-ENDLIST\n")
		if err := os.WriteFile(filepath.Join(mediaDir, cam.Name, "main.m3u8"), []byte(pl.String()), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(&master, "#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d,CODECS=\"%s\"\n%s/main.m3u8\n",
			4_000_000+len(cam.Name)*100_000, cam.Width, cam.Height, cam.Codec, cam.Name)
	}
	if err := os.WriteFile(filepath.Join(mediaDir, "master.m3u8"), []byte(master.String()), 0o644); err != nil {
		return err
	}

	totalDur := float64(opts.Segments) * segDur
	var mpd strings.Builder
	mpd.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	fmt.Fprintf(&mpd, "<MPD type=\"static\" mediaPresentationDuration=\"PT%.3fS\">\n", totalDur)
	fmt.Fprintf(&mpd, "  <Period duration=\"PT%.3fS\">\n", totalDur)
	for _, cam := range cams {
		fmt.Fprintf(&mpd, "    <AdaptationSet id=\"%s\" contentType=\"video\" mimeType=\"video/mp4\" codecs=\"%s\">\n", cam.Name, cam.Codec)
		fmt.Fprintf(&mpd, "      <SegmentTemplate timescale=\"90000\" duration=\"%d\" startNumber=\"0\" media=\"%s/seg$Number$.m4s\" initialization=\"%s/init.mp4\"/>\n",
			opts.FragmentDur, cam.Name, cam.Name)
		fmt.Fprintf(&mpd, "      <Representation id=\"%s\" bandwidth=\"4000000\" width=\"%d\" height=\"%d\" codecs=\"%s\"/>\n",
			cam.Name, cam.Width, cam.Height, cam.Codec)
		mpd.WriteString("    </AdaptationSet>\n")
	}
	mpd.WriteString("  </Period>\n</MPD>\n")
	return os.WriteFile(filepath.Join(mediaDir, "stream.mpd"), []byte(mpd.String()), 0o644)
}
