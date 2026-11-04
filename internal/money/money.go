// Package money holds exact dollar amounts. Comparing spend with a cap in
// floating point can move the verdict, so every amount is an integer.
package money

import (
	"fmt"
	"math"
	"math/big"
	"strings"
)

// Pico is an amount in picodollars (10^-12 USD). int64 holds about $9.2M.
type Pico int64

const (
	perDollar = 1_000_000_000_000
	perMTok   = 1_000_000
)

// ParseUSD parses a non-negative decimal dollar amount such as "0.0045".
// Anything finer than a picodollar is truncated.
func ParseUSD(s string) (Pico, error) {
	return parse(s, perDollar, 1)
}

// ParseRate parses dollars per million tokens, the unit providers publish, and
// returns picodollars per token.
func ParseRate(s string) (Pico, error) {
	return parse(s, perDollar, perMTok)
}

func parse(s string, num, den int64) (Pico, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	if r.Sign() < 0 {
		return 0, fmt.Errorf("amount %q is negative", s)
	}
	r.Mul(r, big.NewRat(num, den))
	n := new(big.Int).Quo(r.Num(), r.Denom())
	if !n.IsInt64() {
		return 0, fmt.Errorf("amount %q is too large", s)
	}
	return Pico(n.Int64()), nil
}

// Add returns a+b and saturates instead of wrapping, because a wrapped ledger
// would turn an enormous overspend into a negative one.
func Add(a, b Pico) Pico {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}

// Mul returns p*n for a non-negative token count, saturating on overflow.
func Mul(p Pico, n int64) Pico {
	if p == 0 || n == 0 {
		return 0
	}
	if p > 0 && n > 0 && int64(p) > math.MaxInt64/n {
		return math.MaxInt64
	}
	return p * Pico(n)
}

// USD returns the amount in dollars for display and JSON. Never compare with it.
func (p Pico) USD() float64 {
	return float64(p) / perDollar
}

// String formats the amount exactly, for example "$0.0045" or "-$1.25".
func (p Pico) String() string {
	sign := ""
	abs := new(big.Int).SetInt64(int64(p))
	if p < 0 {
		sign = "-"
		abs.Neg(abs)
	}
	whole, frac := new(big.Int).QuoRem(abs, big.NewInt(perDollar), new(big.Int))
	digits := strings.TrimRight(fmt.Sprintf("%012d", frac.Int64()), "0")
	for len(digits) < 2 {
		digits += "0"
	}
	return fmt.Sprintf("%s$%s.%s", sign, whole, digits)
}
