// Package money models monetary amounts as integer minor units (kobo).
// Float arithmetic is not representable in this package: amounts are int64
// throughout, so rounding errors cannot occur by construction.
package money

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Kobo is a signed integer count of kobo (1 NGN = 100 kobo).
// It is a distinct named type from int64 so the compiler refuses implicit
// conversion from untyped numeric literals or float expressions.
type Kobo int64

// Parse errors. They are returned wrapped by the service layer so the
// handler can map them to 400-class responses.
var (
	ErrEmpty       = errors.New("amount is empty")
	ErrNotInteger  = errors.New("amount must be integer kobo")
	ErrNonPositive = errors.New("amount must be positive")
	ErrOutOfRange  = errors.New("amount out of range")
)

// ParseKobo accepts a strict integer-string amount like "10000" interpreted
// as kobo. Kept for completeness; the wire payload is naira so the service
// uses ParseNaira. Rejects decimals, scientific notation, signs, whitespace,
// and leading zeros.
func ParseKobo(s string) (Kobo, error) {
	if s == "" {
		return 0, ErrEmpty
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, ErrNotInteger
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return 0, ErrNotInteger
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, ErrOutOfRange
	}
	if n <= 0 {
		return 0, ErrNonPositive
	}
	return Kobo(n), nil
}

// ParseNaira parses a payment amount in naira and returns kobo.
//
// The bank webhook field transaction_amount carries naira. The internal
// type is Kobo, so we convert at the boundary and never let float
// arithmetic touch money.
//
// Accepts:
//
//	"10000"     → 1_000_000 kobo
//	"10000.5"   → 1_000_050 kobo
//	"10000.50"  → 1_000_050 kobo
//	"10000.55"  → 1_000_055 kobo
//
// Rejects: 3+ decimals (no sub-kobo for NGN), scientific notation,
// leading zeros on whole part, signs, whitespace, empty.
func ParseNaira(s string) (Kobo, error) {
	if s == "" {
		return 0, ErrEmpty
	}

	whole := s
	frac := "00"
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		whole = s[:dot]
		frac = s[dot+1:]
		if len(frac) == 0 || len(frac) > 2 {
			return 0, ErrNotInteger
		}
		if len(frac) == 1 {
			frac += "0"
		}
	}

	if whole == "" {
		return 0, ErrNotInteger
	}
	if len(whole) > 1 && whole[0] == '0' {
		return 0, ErrNotInteger
	}
	for i := range len(whole) {
		if whole[i] < '0' || whole[i] > '9' {
			return 0, ErrNotInteger
		}
	}
	for i := range len(frac) {
		if frac[i] < '0' || frac[i] > '9' {
			return 0, ErrNotInteger
		}
	}

	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, ErrOutOfRange
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, ErrOutOfRange
	}

	const maxNaira = int64(9223372036854775807) / 100
	if w > maxNaira {
		return 0, ErrOutOfRange
	}

	total := w*100 + f
	if total <= 0 {
		return 0, ErrNonPositive
	}
	return Kobo(total), nil
}

// String renders the amount as a plain integer, matching input shape.
func (k Kobo) String() string {
	return strconv.FormatInt(int64(k), 10)
}

// Int64 returns the underlying count. Used when writing to SQL BIGINT columns.
func (k Kobo) Int64() int64 {
	return int64(k)
}

// Naira renders the amount as a decimal-naira string with exactly two
// fractional digits. Sign-aware, so a negative drift like -500 kobo
// renders "-5.00". Used in HTTP response bodies; storage stays kobo.
func (k Kobo) Naira() string {
	n := int64(k)
	sign := ""
	if n < 0 {
		sign = "-"
		n = -n
	}
	whole := n / 100
	frac := n % 100
	return fmt.Sprintf("%s%d.%02d", sign, whole, frac)
}
