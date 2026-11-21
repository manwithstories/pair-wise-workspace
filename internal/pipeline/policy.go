package pipeline

import (
	"mkvarchive/internal/audio"
	"mkvarchive/internal/video"
)

func audioRetain(v int) audio.RetentionPolicy {
	if v == PolicyRetain {
		return audio.PolicyRetain
	}
	return audio.PolicyReject
}

func videoRetain(v int) video.RetentionPolicy {
	if v == PolicyRetain {
		return video.PolicyRetain
	}
	return video.PolicyReject
}
