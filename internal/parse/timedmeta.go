package parse

import (
	"mediarun/internal/model"
)

// EventKindOf maps a metadata payload to the model event kind.
func EventKindOf(e any) model.Kind {
	switch e.(type) {
	case SubtitleCue:
		return model.KindSubtitle
	case Chapter:
		return model.KindChapter
	case EmsgEvent:
		return model.KindEmsg
	case SEIEvent:
		return model.KindSEI
	case ID3Tag:
		return model.KindID3
	default:
		return model.KindUnknown
	}
}
