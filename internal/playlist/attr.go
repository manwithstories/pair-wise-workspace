package playlist

import (
	"fmt"
	"strings"
)

// Attribute is one key=value item in an HLS attribute list. Order is kept so
// generated playlists are byte-for-byte deterministic.
type Attribute struct {
	Key    string
	Value  string
	Quoted bool
}

// Attributes is an ordered HLS attribute list.
type Attributes struct {
	items []Attribute
}

// ParseAttributes parses a comma-separated HLS attribute list. Quoted values
// may contain commas and backslash-escaped quotes.
func ParseAttributes(input string) (*Attributes, error) {
	attrs := &Attributes{}
	i := 0
	for {
		for i < len(input) && (input[i] == ' ' || input[i] == '\t') {
			i++
		}
		if i == len(input) {
			return attrs, nil
		}
		start := i
		for i < len(input) && input[i] != '=' && input[i] != ',' {
			i++
		}
		if i == len(input) || input[i] != '=' {
			return nil, fmt.Errorf("attribute at byte %d has no value", start)
		}
		key := strings.TrimSpace(input[start:i])
		if key == "" {
			return nil, fmt.Errorf("attribute at byte %d has empty name", start)
		}
		i++
		var value string
		quoted := false
		if i < len(input) && input[i] == '"' {
			quoted = true
			i++
			var b strings.Builder
			closed := false
			for i < len(input) {
				c := input[i]
				i++
				if c == '\\' && i < len(input) {
					b.WriteByte(input[i])
					i++
					continue
				}
				if c == '"' {
					closed = true
					break
				}
				b.WriteByte(c)
			}
			if !closed {
				return nil, fmt.Errorf("unterminated quoted value for %s", key)
			}
			value = b.String()
		} else {
			start = i
			for i < len(input) && input[i] != ',' {
				i++
			}
			value = strings.TrimSpace(input[start:i])
		}
		attrs.items = append(attrs.items, Attribute{Key: key, Value: value, Quoted: quoted})
		if i == len(input) {
			return attrs, nil
		}
		if input[i] != ',' {
			return nil, fmt.Errorf("attribute %s has trailing data", key)
		}
		i++
	}
}

// NewAttributes constructs an ordered list from pairs.
func NewAttributes(items ...Attribute) *Attributes {
	return &Attributes{items: append([]Attribute(nil), items...)}
}

// Add appends or replaces an attribute while retaining first-insertion order.
func (a *Attributes) Add(key, value string, quoted bool) {
	for i := range a.items {
		if a.items[i].Key == key {
			a.items[i] = Attribute{Key: key, Value: value, Quoted: quoted}
			return
		}
	}
	a.items = append(a.items, Attribute{Key: key, Value: value, Quoted: quoted})
}

// Get returns the value for key.
func (a *Attributes) Get(key string) (string, bool) {
	for _, item := range a.items {
		if item.Key == key {
			return item.Value, true
		}
	}
	return "", false
}

// Len reports the number of attributes.
func (a *Attributes) Len() int {
	if a == nil {
		return 0
	}
	return len(a.items)
}

// Items returns a copy of the ordered attributes.
func (a *Attributes) Items() []Attribute {
	if a == nil {
		return nil
	}
	return append([]Attribute(nil), a.items...)
}

// String serializes the list without surrounding tag text.
func (a *Attributes) String() string {
	if a == nil || len(a.items) == 0 {
		return ""
	}
	parts := make([]string, 0, len(a.items))
	for _, item := range a.items {
		value := item.Value
		if item.Quoted {
			value = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
		}
		parts = append(parts, item.Key+"="+value)
	}
	return strings.Join(parts, ",")
}

// ParseQuotedValue unquotes one value from a tag suffix.
func ParseQuotedValue(value string) (string, error) {
	attrs, err := ParseAttributes("V=" + value)
	if err != nil {
		return "", err
	}
	out, _ := attrs.Get("V")
	return out, nil
}
