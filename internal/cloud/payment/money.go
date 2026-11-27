package payment

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// FenPerYuan is the number of fen in one CNY.
	FenPerYuan int64 = 100

	// MicroCNYPerYuan is the wallet's unit: one CNY is 1,000,000 micros.
	MicroCNYPerYuan int64 = 1_000_000

	// MicroCNYPerFen is one fen in the wallet's unit.
	MicroCNYPerFen int64 = MicroCNYPerYuan / FenPerYuan

	// MaxAmountFen bounds one recharge at 10,000,000 CNY. It keeps every
	// micro-CNY conversion far inside int64 and refuses a typo that would
	// otherwise create an absurd order.
	MaxAmountFen Fen = 1_000_000_000
)

// Fen is an amount in CNY cents, the unit Alipay and WeChat Pay bill in.
type Fen int64

// MicroCNY is an amount in the wallet's micro-CNY unit.
type MicroCNY int64

// MicroCNY converts a fen amount to micro-CNY. The caller has already checked
// the bound, so the multiplication cannot overflow.
func (f Fen) MicroCNY() MicroCNY {
	return MicroCNY(int64(f) * MicroCNYPerFen)
}

// YuanString renders fen as a decimal CNY string with exactly two fraction
// digits, the format Alipay's total_amount uses. It never uses a float.
func (f Fen) YuanString() string {
	return fmt.Sprintf("%d.%02d", int64(f)/FenPerYuan, int64(f)%FenPerYuan)
}

// FenFromYuan parses a decimal CNY string such as "0.01", "10" or "10.50"
// into fen without going through a float. One or two fraction digits are
// accepted; anything else, a sign, a space, a zero amount or an amount above
// MaxAmountFen is refused.
func FenFromYuan(s string) (Fen, error) {
	if len(s) > 32 {
		return 0, fmt.Errorf("%w: amount is longer than 32 bytes", ErrInvalidAmount)
	}
	if s == "" {
		return 0, fmt.Errorf("%w: empty amount", ErrInvalidAmount)
	}
	intPart, fracPart, hasDot := strings.Cut(s, ".")
	if hasDot && strings.Contains(fracPart, ".") {
		return 0, fmt.Errorf("%w: %q has more than one decimal point", ErrInvalidAmount, s)
	}
	if intPart == "" || len(fracPart) > 2 {
		return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	if hasDot && fracPart == "" {
		return 0, fmt.Errorf("%w: %q has an empty fraction", ErrInvalidAmount, s)
	}
	for _, r := range intPart {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%w: %q is not a decimal amount", ErrInvalidAmount, s)
		}
	}
	for _, r := range fracPart {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%w: %q is not a decimal amount", ErrInvalidAmount, s)
		}
	}
	yuan, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	if yuan > int64(MaxAmountFen)/FenPerYuan {
		return 0, fmt.Errorf("%w: %q is above the maximum", ErrInvalidAmount, s)
	}
	frac := int64(0)
	for _, r := range fracPart {
		frac = frac*10 + int64(r-'0')
	}
	if len(fracPart) == 1 {
		frac *= 10
	}
	fen := Fen(yuan*FenPerYuan + frac)
	if fen <= 0 || fen > MaxAmountFen {
		return 0, fmt.Errorf("%w: %q is outside 0.01..%s", ErrInvalidAmount, s, MaxAmountFen.YuanString())
	}
	return fen, nil
}

// validAmount reports whether f is a positive amount within the bound.
func validAmount(f Fen) bool { return f > 0 && f <= MaxAmountFen }
