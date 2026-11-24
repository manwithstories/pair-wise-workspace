package ingest

import (
	"mediarun/internal/model"
)

// spillQueue is the durable backlog behind the in-memory queue.
type spillQueue interface {
	Write(events []model.Event) (int, error)
	Read(n int) ([]model.Event, error)
	Len() int
	Close() error
}
