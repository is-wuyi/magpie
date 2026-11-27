package payment

import (
	"errors"
	"testing"
)

// Alipay's total_amount is a decimal string. Parsing it must be exact: no
// float, no rounding, no acceptance of one and a half fen.
func TestFenFromYuanIsExact(t *testing.T) {
	valid := []struct {
		in   string
		want Fen
	}{
		{"0.01", 1},
		{"0.1", 10},
		{"1", 100},
		{"1.2", 120},
		{"1.20", 120},
		{"10.05", 1005},
		{"10000000.00", MaxAmountFen},
	}
	for _, tc := range valid {
		got, err := FenFromYuan(tc.in)
		if err != nil {
			t.Errorf("FenFromYuan(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("FenFromYuan(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}

	invalid := []string{
		"", "0", "0.00", "-1", "+1", "1.234", "1.", ".5", " 1", "1 ",
		"1,00", "abc", "1e2", "0.001", "10000000.01", "--1", "1.-1",
	}
	for _, in := range invalid {
		if got, err := FenFromYuan(in); err == nil {
			t.Errorf("FenFromYuan(%q) = %d, want an error", in, got)
		} else if !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("FenFromYuan(%q) error = %v, want ErrInvalidAmount", in, err)
		}
	}
}

func TestFenYuanAndMicros(t *testing.T) {
	cases := []struct {
		fen   Fen
		yuan  string
		micro MicroCNY
	}{
		{1, "0.01", 10_000},
		{100, "1.00", 1_000_000},
		{999, "9.99", 9_990_000},
		{MaxAmountFen, "10000000.00", 10_000_000_000_000},
	}
	for _, tc := range cases {
		if got := tc.fen.YuanString(); got != tc.yuan {
			t.Errorf("%d fen renders %q, want %q", tc.fen, got, tc.yuan)
		}
		if got := tc.fen.MicroCNY(); got != tc.micro {
			t.Errorf("%d fen is %d micro-CNY, want %d", tc.fen, got, tc.micro)
		}
	}
}

// A preset is the only way an amount enters an order, and its fields are
// checked when the Service is built.
func TestPresetsAreValidated(t *testing.T) {
	bad := []struct {
		name    string
		presets []Preset
	}{
		{"empty", nil},
		{"zero amount", []Preset{{ID: "a", AmountFen: 0, Label: "x"}}},
		{"negative amount", []Preset{{ID: "a", AmountFen: -1, Label: "x"}}},
		{"too large", []Preset{{ID: "a", AmountFen: MaxAmountFen + 1, Label: "x"}}},
		{"bad id", []Preset{{ID: "A", AmountFen: 100, Label: "x"}}},
		{"empty label", []Preset{{ID: "a", AmountFen: 100, Label: ""}}},
		{"control label", []Preset{{ID: "a", AmountFen: 100, Label: "a\nb"}}},
		{"duplicate id", []Preset{{ID: "a", AmountFen: 100, Label: "x"}, {ID: "a", AmountFen: 200, Label: "y"}}},
	}
	for _, tc := range bad {
		if _, err := newPresetSet(tc.presets); err == nil {
			t.Errorf("%s: newPresetSet accepted the presets", tc.name)
		}
	}
	if _, err := newPresetSet([]Preset{{ID: "p10", AmountFen: 1000, Label: "10元"}}); err != nil {
		t.Fatalf("valid preset: %v", err)
	}
}
