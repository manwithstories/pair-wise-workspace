package pipeline

import "mediamerge/internal/clock"

func metadataTimebase() clock.Timebase { return clock.Must(48000, 1) }
