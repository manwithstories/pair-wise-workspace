package clock

import "errors"

// List is an ordered, deduplicating bag of classified errors. It is the
// hand-off format between subsystems and the report writer.
type List struct {
	items []*Error
	seen  map[string]struct{}
}

// Add classifies and appends err unless it is nil. Duplicate (category,
// span, message) triples are collapsed so a retried recover pass cannot
// double-count the same defect.
func (l *List) Add(err error) {
	if err == nil {
		return
	}
	var ce *Error
	if !errors.As(err, &ce) {
		ce = &Error{Category: CatInternal, Message: err.Error(), Err: err}
	}
	if l.seen == nil {
		l.seen = make(map[string]struct{})
	}
	key := string(ce.Category) + "|" + ce.Span.String() + "|" + ce.Message
	if _, dup := l.seen[key]; dup {
		return
	}
	l.seen[key] = struct{}{}
	l.items = append(l.items, ce)
}

// Items returns the collected errors in insertion order.
func (l *List) Items() []*Error { return l.items }

// Len returns the number of distinct collected errors.
func (l *List) Len() int { return len(l.items) }

// CountByCategory counts the distinct errors per category.
func (l *List) CountByCategory() map[Category]int {
	out := make(map[Category]int, len(l.items))
	for _, it := range l.items {
		out[it.Category]++
	}
	return out
}

// AnyRecoverable reports whether at least one error was handled with a
// non-None recovery action.
func (l *List) AnyRecoverable() bool {
	for _, it := range l.items {
		if it.Recovery != RecoveredNone {
			return true
		}
	}
	return false
}

// AnyUnrecoverable reports whether any error stops the unit entirely.
func (l *List) AnyUnrecoverable() bool {
	for _, it := range l.items {
		if it.Recovery == RecoveredNone {
			return true
		}
	}
	return false
}

// Err joins the errors into one error value or returns nil when empty.
func (l *List) Err() error {
	if len(l.items) == 0 {
		return nil
	}
	errs := make([]error, len(l.items))
	for i, it := range l.items {
		errs[i] = it
	}
	return errors.Join(errs...)
}
