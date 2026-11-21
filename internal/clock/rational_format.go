package clock

import (
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

func Parse(s string) (Rational, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rational{}, errors.New("clock: empty rational")
	}
	if strings.Contains(s, "/") {
		parts := strings.Split(s, "/")
		if len(parts) != 2 {
			return Rational{}, fmt.Errorf("clock: malformed rational %q", s)
		}
		n, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		d, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err1 != nil || err2 != nil {
			return Rational{}, fmt.Errorf("clock: malformed rational %q", s)
		}
		return New(n, d)
	}
	x, ok := new(big.Rat).SetString(s)
	if !ok {
		return Rational{}, fmt.Errorf("clock: malformed decimal %q", s)
	}
	return fromBig(x)
}

func (r Rational) Milliseconds() float64 { return r.Float64() * 1000 }

// Trunc returns the value truncated toward zero.
func (r Rational) Trunc() int64 {
	c := r.canonical()
	return c.Num / c.Den
}

func (r Rational) Round() int64 {
	c := r.canonical()
	q := c.Num / c.Den
	rem := c.Num % c.Den
	if rem == 0 {
		return q
	}
	absRem := rem
	if absRem < 0 {
		absRem = -absRem
	}
	// absRem*2 >= c.Den, written to avoid overflow.
	if absRem >= c.Den-absRem {
		if c.Num > 0 {
			q++
		} else {
			q--
		}
	}
	return q
}

func (r Rational) Rescale(unitsPerSecond int64) (int64, error) {
	if unitsPerSecond <= 0 {
		return 0, fmt.Errorf("clock: invalid rescale base %d", unitsPerSecond)
	}
	x := new(big.Rat).Mul(r.big(), new(big.Rat).SetInt64(unitsPerSecond))
	num, den := x.Num(), x.Denom()
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if new(big.Int).Lsh(new(big.Int).Abs(rem), 1).Cmp(den) >= 0 {
		if num.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	if !q.IsInt64() {
		return 0, ErrOverflow
	}
	return q.Int64(), nil
}

func (r Rational) String() string {
	c := r.canonical()
	if c.Den == 1 {
		return strconv.FormatInt(c.Num, 10)
	}
	return strconv.FormatInt(c.Num, 10) + "/" + strconv.FormatInt(c.Den, 10)
}

func (r *Rational) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	v, err := Parse(s)
	if err != nil {
		return err
	}
	*r = v
	return nil
}

func (r Rational) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(r.String())), nil
}
