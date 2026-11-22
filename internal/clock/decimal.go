package clock

import (
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// ParseDecimalTicks parses a non-negative decimal seconds value such as
// "1.96" or "10" into exact ticks without going through float64.
func ParseDecimalTicks(s string, timescale uint32) (int64, error) {
	if timescale == 0 || timescale > maxTimescale {
		return 0, fmt.Errorf("%w: %d", ErrInvalidTimebase, timescale)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("clock: empty decimal seconds")
	}
	neg := false
	if s[0] == '+' || s[0] == '-' {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" {
		return 0, errors.New("clock: missing decimal seconds")
	}
	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return 0, fmt.Errorf("clock: invalid decimal seconds %q", s)
	}
	intPart := parts[0]
	if intPart == "" {
		intPart = "0"
	}
	if !allDigits(intPart) {
		return 0, fmt.Errorf("clock: invalid decimal seconds %q", s)
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("clock: decimal seconds overflow: %w", err)
	}
	ticksBig := new(big.Int).Mul(big.NewInt(whole), big.NewInt(int64(timescale)))
	if len(parts) == 2 {
		frac := parts[1]
		if frac == "" || !allDigits(frac) {
			return 0, fmt.Errorf("clock: invalid decimal fraction %q", parts[1])
		}
		num, ok := new(big.Int).SetString(frac, 10)
		if !ok {
			return 0, fmt.Errorf("clock: invalid decimal fraction %q", parts[1])
		}
		num.Mul(num, big.NewInt(int64(timescale)))
		den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(frac))), nil)
		q, r := new(big.Int).QuoRem(num, den, new(big.Int))
		if new(big.Int).Lsh(r, 1).Cmp(den) >= 0 {
			q.Add(q, big.NewInt(1))
		}
		ticksBig.Add(ticksBig, q)
	}
	if neg {
		ticksBig.Neg(ticksBig)
	}
	if !ticksBig.IsInt64() {
		return 0, errors.New("clock: decimal seconds overflow")
	}
	return ticksBig.Int64(), nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// FormatDecimalTicks renders ticks as seconds with exactly decimals digits.
// The result is rounded half away from zero and is never "-0.000".
func FormatDecimalTicks(ticks int64, timescale uint32, decimals int) string {
	if decimals < 0 {
		decimals = 0
	}
	if decimals > 9 {
		decimals = 9
	}
	if timescale == 0 {
		return "0"
	}
	neg := ticks < 0
	abs := ticks
	if neg {
		abs = -abs
	}
	scale := int64(1)
	for i := 0; i < decimals; i++ {
		scale *= 10
	}
	num := new(big.Int).Mul(big.NewInt(abs), big.NewInt(scale))
	q, r := new(big.Int).QuoRem(num, big.NewInt(int64(timescale)), new(big.Int))
	if new(big.Int).Lsh(r, 1).Cmp(big.NewInt(int64(timescale))) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	s := q.String()
	if decimals > 0 {
		for len(s) <= decimals {
			s = "0" + s
		}
		s = s[:len(s)-decimals] + "." + s[len(s)-decimals:]
	}
	if neg && q.Sign() != 0 {
		s = "-" + s
	}
	return s
}
