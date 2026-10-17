// Package money parses and formats exact decimal amounts as integer minor units.
// Floats are never used: "−1234.5600" at scale 2 becomes -123456.
package money

import (
	"fmt"
	"strings"
)

// ParseMinor parses a decimal string into minor units at the given scale.
// Extra fractional digits are allowed only if they are zeros (exact conversion).
func ParseMinor(s string, scale int) (int64, error) {
	return parse(s, scale, false)
}

// ParseMinorRound is like ParseMinor but rounds extra fractional digits half away from zero.
// Used only where the connector supplies more precision than the commodity scale (e.g. cost basis).
func ParseMinorRound(s string, scale int) (int64, error) {
	return parse(s, scale, true)
}

func parse(s string, scale int, round bool) (int64, error) {
	orig := s
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("money: empty amount")
	}
	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i+1:]
	}
	if intPart == "" {
		intPart = "0"
	}
	if !digits(intPart) || !digits(frac) {
		return 0, fmt.Errorf("money: invalid amount %q", orig)
	}
	roundUp := false
	if len(frac) > scale {
		extra := frac[scale:]
		frac = frac[:scale]
		if strings.Trim(extra, "0") != "" {
			if !round {
				return 0, fmt.Errorf("money: %q has more than %d decimal places", orig, scale)
			}
			roundUp = extra[0] >= '5'
		}
	}
	frac += strings.Repeat("0", scale-len(frac))
	var v int64
	for _, c := range intPart + frac {
		if v > (1<<62)/10 {
			return 0, fmt.Errorf("money: %q overflows", orig)
		}
		v = v*10 + int64(c-'0')
	}
	if roundUp {
		v++
	}
	if neg {
		v = -v
	}
	return v, nil
}

func digits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Format renders minor units as a plain decimal string ("-1234.56").
func Format(v int64, scale int) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%0*d", scale+1, v)
	if scale > 0 {
		s = s[:len(s)-scale] + "." + s[len(s)-scale:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

// FormatUSD renders cents with thousands separators ("-1,234.56").
func FormatUSD(cents int64) string {
	s := Format(cents, 2)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	dot := strings.IndexByte(s, '.')
	ip, fp := s[:dot], s[dot:]
	var b strings.Builder
	for i, c := range ip {
		if i > 0 && (len(ip)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := b.String() + fp
	if neg {
		out = "-" + out
	}
	return out
}
