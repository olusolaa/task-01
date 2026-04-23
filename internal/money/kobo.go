// Package money models monetary amounts as integer minor units (kobo).
// Float arithmetic is not representable in this package: amounts are int64
// throughout, so rounding errors cannot occur by construction.
package money

import (
	"errors"
	"strconv"
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

// ParseKobo accepts a strict integer-string amount like "10000".
// It rejects decimals, scientific notation, signs, whitespace, and leading
// zeros. The string format is chosen for us in the payload; the strictness
// is our policy.
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

// String renders the amount as a plain integer, matching input shape.
func (k Kobo) String() string {
	return strconv.FormatInt(int64(k), 10)
}

// Int64 returns the underlying count. Used when writing to SQL BIGINT columns.
func (k Kobo) Int64() int64 {
	return int64(k)
}
