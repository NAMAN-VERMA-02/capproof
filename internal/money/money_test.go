package money

import (
	"math"
	"testing"
)

func TestParseUSD(t *testing.T) {
	tests := []struct {
		in      string
		want    Pico
		wantErr bool
	}{
		{in: "0.0045", want: 4_500_000_000},
		{in: " 1.5 ", want: 1_500_000_000_000},
		{in: "0", want: 0},
		{in: "0.0000000000009", want: 0}, // below one picodollar
		{in: "1e-3", want: 1_000_000_000},
		{in: "", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "NaN", wantErr: true},
		{in: "Inf", wantErr: true},
		{in: "-1", wantErr: true},
		{in: "99999999", wantErr: true}, // would overflow int64 picodollars
	}
	for _, tt := range tests {
		got, err := ParseUSD(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseUSD(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
		}
		if got != tt.want {
			t.Errorf("ParseUSD(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestParseRateIsExact(t *testing.T) {
	// A float would turn 0.15 into 149999.99...; the rate must be exact.
	got, err := ParseRate("0.15")
	if err != nil || got != 150_000 {
		t.Fatalf("ParseRate(0.15) = %d, %v; want 150000", got, err)
	}
}

func TestAddAndMulSaturate(t *testing.T) {
	if got := Add(math.MaxInt64, 1); got != math.MaxInt64 {
		t.Errorf("Add overflow = %d", got)
	}
	if got := Add(math.MinInt64, -1); got != math.MinInt64 {
		t.Errorf("Add underflow = %d", got)
	}
	if got := Mul(math.MaxInt64, 2); got != math.MaxInt64 {
		t.Errorf("Mul overflow = %d", got)
	}
	if got := Mul(150_000, 1000); got != 150_000_000 {
		t.Errorf("Mul = %d", got)
	}
	if got := Mul(0, 1<<62); got != 0 {
		t.Errorf("Mul by zero rate = %d", got)
	}
}

func TestString(t *testing.T) {
	tests := map[Pico]string{
		0:                  "$0.00",
		4_500_000_000:      "$0.0045",
		1_500_000_000_000:  "$1.50",
		1:                  "$0.000000000001",
		-1_250_000_000_000: "-$1.25",
		math.MinInt64:      "-$9223372.036854775808",
	}
	for in, want := range tests {
		if got := in.String(); got != want {
			t.Errorf("Pico(%d).String() = %q, want %q", int64(in), got, want)
		}
	}
}
