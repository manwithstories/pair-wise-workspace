package reviewapp

import "review/internal/media"

func sentinelBoundary() error {
	return media.NewBoundary("test", "boundary case", 0, -1, nil)
}

func sentinelInternal() error {
	return media.NewInternal("test", "internal case", nil)
}
