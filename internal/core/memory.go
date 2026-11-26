package core

// MaxProcessMemory is the project's conservative local working-set budget.
// It is not a substitute for the OS, but every large allocation is checked
// against it before being made.
const MaxProcessMemory int64 = 64 << 20

// MaxCachedFrames is the maximum number of sample frames the repair executor
// may retain in memory at one time.
const MaxCachedFrames int = 65536

// CheckAllocation rejects buffers outside the documented 64 MiB budget.
func CheckAllocation(op string, bytes int64) error {
	if bytes < 0 {
		return Newf(KindMemoryLimit, op, "negative allocation size %d", bytes)
	}
	if bytes > MaxProcessMemory {
		return Newf(KindMemoryLimit, op,
			"allocation %d bytes exceeds %d byte (64 MiB) limit", bytes, MaxProcessMemory)
	}
	return nil
}

// CheckFrameCache validates a frame cache against both the frame-count and
// byte-size limits.
func CheckFrameCache(op string, frames, channels, bytesPerSample int) (int64, error) {
	if frames < 0 || channels <= 0 || bytesPerSample <= 0 {
		return 0, Newf(KindInvalidArgument, op,
			"invalid cache dimensions frames=%d channels=%d bps=%d", frames, channels, bytesPerSample)
	}
	if frames > MaxCachedFrames {
		return 0, Newf(KindMemoryLimit, op,
			"frame cache %d exceeds maximum %d frames", frames, MaxCachedFrames)
	}
	n := int64(frames) * int64(channels) * int64(bytesPerSample)
	if err := CheckAllocation(op, n); err != nil {
		return 0, err
	}
	return n, nil
}
