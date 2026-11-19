package fixture

import (
	"fmt"
	"os"
	"path/filepath"

	"review/internal/clock"
)

// Generate writes the full demo input set: one SRT, one WebVTT, one
// timecode CSV, one RTP stream and one MP4 per camera. Damage injections
// are spread across cameras: cam1 gets the truncated/dirty SRT and the RTP
// anomalies, cam2 the tail-truncated MP4, cam3 the incomplete index.
func Generate(work string, cams int, r clock.Rate, framesPerCam int64, inject []string) error {
	has := func(s string) bool {
		for _, x := range inject {
			if x == s {
				return true
			}
		}
		return false
	}
	dir := filepath.Join(work, "inputs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for cam := 0; cam < cams; cam++ {
		base := filepath.Join(dir, fmt.Sprintf("cam%02d", cam))
		dirty := cam == 1
		truncSRT := dirty && has("truncated-srt")
		if err := os.WriteFile(base+".srt", GenSRT(cam, r, 16, dirty, truncSRT), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(base+".vtt", GenVTT(cam, r, 8), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(base+".tc.csv", GenTimecodes(r, 40), 0o644); err != nil {
			return err
		}
		if err := GenRTP(base+".rtp", int(framesPerCam), r, dirty); err != nil {
			return err
		}
		damage := ""
		switch {
		case cam == 2 && has("damaged-mp4"):
			damage = "truncated"
		case cam == 3 && has("incomplete-index"):
			damage = "incomplete-index"
		}
		if _, err := GenMP4(base+".mp4", int(framesPerCam), r, cam, damage); err != nil {
			return err
		}
	}
	return nil
}
