package psi

import "fmt"

const (
	DescriptorRegistration = 0x05
	DescriptorLanguage     = 0x0a
)

// Descriptor is a generically parsed MPEG-TS/DVB descriptor. Language and
// registration fields are filled for their well-known tags.
type Descriptor struct {
	Tag          uint8  `json:"tag"`
	Length       int    `json:"length"`
	Language     string `json:"language,omitempty"`
	AudioType    *uint8 `json:"audio_type,omitempty"`
	Registration string `json:"registration,omitempty"`
	Data         []byte `json:"data,omitempty"`
}

// ParseDescriptors walks a descriptor loop and rejects truncation.
func ParseDescriptors(b []byte) ([]Descriptor, error) {
	var out []Descriptor
	for len(b) > 0 {
		if len(b) < 2 {
			return out, fmt.Errorf("psi: truncated descriptor header")
		}
		tag, n := b[0], int(b[1])
		if 2+n > len(b) {
			return out, fmt.Errorf("psi: descriptor 0x%02x length %d exceeds loop", tag, n)
		}
		d := Descriptor{Tag: tag, Length: n, Data: append([]byte(nil), b[2:2+n]...)}
		if tag == DescriptorLanguage && n >= 3 {
			d.Language = string(b[2:5])
			if n >= 4 {
				audioType := b[5]
				d.AudioType = &audioType
			}
		}
		if tag == DescriptorRegistration && n >= 4 {
			d.Registration = string(b[2:6])
		}
		out = append(out, d)
		b = b[2+n:]
	}
	return out, nil
}

// Languages returns ISO-639 language codes in the descriptor loop.
func Languages(ds []Descriptor) []string {
	var out []string
	for _, d := range ds {
		if d.Language != "" {
			out = append(out, d.Language)
		}
	}
	return out
}

// Registrations returns format identifiers in the descriptor loop.
func Registrations(ds []Descriptor) []string {
	var out []string
	for _, d := range ds {
		if d.Registration != "" {
			out = append(out, d.Registration)
		}
	}
	return out
}

// LanguageDescriptor builds an ISO-639 language descriptor.
func LanguageDescriptor(lang string, audioType byte) []byte {
	for len(lang) < 3 {
		lang += " "
	}
	return []byte{DescriptorLanguage, 4, lang[0], lang[1], lang[2], audioType}
}

// RegistrationDescriptor builds a registration descriptor.
func RegistrationDescriptor(id string) []byte {
	for len(id) < 4 {
		id += " "
	}
	return []byte{DescriptorRegistration, 4, id[0], id[1], id[2], id[3]}
}
