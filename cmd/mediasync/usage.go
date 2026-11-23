package main

import (
	"fmt"
	"io"
)

func usage(w io.Writer) {
	fmt.Fprintln(w, `mediasync — MXF/MP4/RTP media package builder

  mediasync demo [flags]

Run the end-to-end demo against generated fixtures. No network access and no
external sample files are used.

Flags:
  --seed int               fixture seed (default 41)
  --loss-burst int         RTP packets lost in one burst (default 7)
  --duplicate int          RTP packets transmitted twice (default 3)
  --reorder int            RTP reordering distance (default 5)
  --bandwidth-kbps int     seed bitrate estimate (default 1800)
  --target-kbps int        declared bitrate target (default 2000)
  --checkpoint-samples int samples per checkpoint block (default 120)
  --resume                 interrupt once, then resume from checkpoints
  --corrupt string         damaged input: truncated-mp4 | invalid-klv
  --rotation int           explicit rotation override (0, 90, 180, 270)`)
}
