package subtitle

import (
	"strings"

	"archivetl/internal/diag"
)

// parseStamp accepts HH:MM:SS.mmm, MM:SS.mmm and SRT's comma fraction and
// returns whole milliseconds. It lives separately from the streaming parser
// so the line I/O and time grammar stay independently reviewable.
func parseStamp(s string) (int64, error) {
	s = strings.ReplaceAll(s, ",", ".")
	cols := strings.Split(s, ":")
	if len(cols) < 2 || len(cols) > 3 {
		return 0, diag.ErrInvalid
	}
	var hh, mm int64
	var secStr string
	var err error
	if len(cols) == 3 {
		if hh, err = parseInt(cols[0]); err != nil {
			return 0, err
		}
		mm, err = parseInt(cols[1])
	} else {
		mm, err = parseInt(cols[0])
	}
	if err != nil {
		return 0, err
	}
	secStr = cols[len(cols)-1]
	dot := strings.IndexByte(secStr, '.')
	var ss, frac int64
	if dot < 0 {
		ss, err = parseInt(secStr)
		frac = 0
	} else {
		ss, err = parseInt(secStr[:dot])
		if err == nil {
			f := secStr[dot+1:]
			if len(f) == 0 || len(f) > 3 {
				return 0, diag.ErrInvalid
			}
			for len(f) < 3 {
				f += "0"
			}
			frac, err = parseInt(f)
		}
	}
	if err != nil || mm > 59 || ss > 59 {
		return 0, diag.ErrInvalid
	}
	return ((hh*60+mm)*60+ss)*1000 + frac, nil
}

func parseInt(s string) (int64, error) {
	if s == "" {
		return 0, diag.ErrInvalid
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, diag.ErrInvalid
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}
