package media

// Hard limits shared across modules. They are intentionally conservative so
// that a hostile manifest or trace cannot exhaust memory.
const (
	// MaxManifestBytes is the maximum accepted manifest size (1 MiB).
	MaxManifestBytes = 1 << 20
	// MaxSegmentBytes is the maximum accepted single segment size (8 MiB).
	MaxSegmentBytes = 8 << 20
	// MaxWorkers bounds the configurable worker pool.
	MaxWorkers = 32
	// MaxQueue bounds the configurable task queue depth.
	MaxQueue = 4096
	// MaxRepresentations bounds DASH adaptation sets / HLS variant levels.
	MaxRepresentations = 16
	// MaxSegments bounds segments listed in a single manifest.
	MaxSegments = 1024
	// MaxJitterPackets bounds the jitter buffer occupancy.
	MaxJitterPackets = 8192
	// MaxTracks bounds tracks inside one fragmented segment.
	MaxTracks = 8
)
