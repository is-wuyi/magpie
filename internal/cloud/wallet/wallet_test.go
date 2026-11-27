package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yetone/magpie/internal/cloud/pricing"
)

func TestNewRejectsNilPool(t *testing.T) {
	if _, err := New(nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("New(nil) error = %v, want ErrInvalidRequest", err)
	}
}

func TestOpenRejectsEmptyURL(t *testing.T) {
	for _, url := range []string{"", "   "} {
		if _, err := Open(context.Background(), url); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Open(%q) error = %v, want ErrInvalidRequest", url, err)
		}
	}
}

func TestZeroValueServiceIsRefused(t *testing.T) {
	var svc Service
	ctx := context.Background()
	if _, err := svc.Balance(ctx, uuid.New()); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Balance on zero Service = %v, want ErrInvalidRequest", err)
	}
	if _, err := svc.Credit(ctx, uuid.New(), 1, "key", ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Credit on zero Service = %v, want ErrInvalidRequest", err)
	}
	if _, err := svc.Reserve(ctx, ReserveRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Reserve on zero Service = %v, want ErrInvalidRequest", err)
	}
	if _, err := svc.Settle(ctx, SettleRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Settle on zero Service = %v, want ErrInvalidRequest", err)
	}
	if _, err := svc.Refund(ctx, RefundRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Refund on zero Service = %v, want ErrInvalidRequest", err)
	}
	if _, err := svc.RecoverStaleHolds(ctx, time.Now(), 1); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("RecoverStaleHolds on zero Service = %v, want ErrInvalidRequest", err)
	}
}

func TestCreditResultJSONMatchesTheCloudAPI(t *testing.T) {
	data, err := json.Marshal(CreditResult{Replayed: true})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"entryId", "userId", "amountMicros", "balanceMicros", "replayed"} {
		if _, ok := fields[name]; !ok {
			t.Errorf("credit response has no %q: %s", name, data)
		}
	}
	if _, ok := fields["Replayed"]; ok {
		t.Errorf("credit response used Go's default field name: %s", data)
	}
}

func TestValidateReserveRejectsBadInput(t *testing.T) {
	quote, err := pricing.FreeQuote(pricing.Key{Provider: "openai", Model: "gpt-5"})
	if err != nil {
		t.Fatalf("FreeQuote: %v", err)
	}
	s := &Service{}
	valid := ReserveRequest{UserID: uuid.New(), RequestID: "r1", Quote: quote}
	if err := s.validateReserve(valid); err != nil {
		t.Fatalf("valid request: %v", err)
	}

	tests := []struct {
		name string
		req  ReserveRequest
		want error
	}{
		{"empty user", ReserveRequest{RequestID: "r1", Quote: quote}, ErrInvalidRequest},
		{"empty request id", ReserveRequest{UserID: uuid.New(), Quote: quote}, ErrInvalidRequest},
		{"blank request id", ReserveRequest{UserID: uuid.New(), RequestID: "  ", Quote: quote}, ErrInvalidRequest},
		{"negative ttl", ReserveRequest{UserID: uuid.New(), RequestID: "r1", Quote: quote, TTL: -time.Second}, ErrInvalidRequest},
		{"long description", ReserveRequest{UserID: uuid.New(), RequestID: "r1", Quote: quote, Description: strings.Repeat("x", MaxDescriptionLength+1)}, ErrInvalidRequest},
		{"zero quote", ReserveRequest{UserID: uuid.New(), RequestID: "r1"}, pricing.ErrInvalidKey},
		{"negative estimated usage", ReserveRequest{UserID: uuid.New(), RequestID: "r1", Quote: quote, EstimatedUsage: pricing.Usage{InputTokens: -1}}, pricing.ErrInvalidUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := s.validateReserve(tt.req); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestCreditKeyRefusesInternalPrefix(t *testing.T) {
	if err := validateCreditKey(""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty key error = %v, want ErrInvalidRequest", err)
	}
	if err := validateCreditKey(internalIDKeyPrefix + "123:hold"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("prefixed key error = %v, want ErrInvalidRequest", err)
	}
	if err := validateCreditKey("stripe:evt_123"); err != nil {
		t.Fatalf("provider key: %v", err)
	}
}

func TestMoneyArithmeticRefusesOverflow(t *testing.T) {
	if got, err := addInt64(math.MaxInt64-1, 1); err != nil || got != math.MaxInt64 {
		t.Fatalf("add to Max = %d, %v", got, err)
	}
	if _, err := addInt64(math.MaxInt64, 1); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("Max+1 error = %v", err)
	}
	if _, err := addInt64(math.MinInt64, -1); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("Min-1 error = %v", err)
	}
	if got, err := subInt64(0, -1); err != nil || got != 1 {
		t.Fatalf("0-(-1) = %d, %v", got, err)
	}
	if _, err := subInt64(math.MinInt64, 1); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("Min-1 error = %v", err)
	}
	if _, err := subInt64(math.MaxInt64, -1); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("Max-(-1) error = %v", err)
	}
}

func TestUnusedHold(t *testing.T) {
	tests := []struct {
		hold, charge, want pricing.Micros
	}{
		{100, 40, 60},
		{100, 100, 0},
		{100, 140, 0},
		{0, 0, 0},
		{0, 10, 0},
	}
	for _, tt := range tests {
		if got := unusedHold(tt.hold, tt.charge); got != tt.want {
			t.Fatalf("unusedHold(%d, %d) = %d, want %d", tt.hold, tt.charge, got, tt.want)
		}
	}
}

func TestValidateTextBounds(t *testing.T) {
	if err := validateRequiredText("request id", strings.Repeat("a", MaxRequestIDLength), MaxRequestIDLength); err != nil {
		t.Fatalf("exactly at bound: %v", err)
	}
	if err := validateRequiredText("request id", strings.Repeat("a", MaxRequestIDLength+1), MaxRequestIDLength); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("over bound error = %v, want ErrInvalidRequest", err)
	}
	if err := validateRequiredText("request id", "a\x00b", MaxRequestIDLength); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("NUL error = %v, want ErrInvalidRequest", err)
	}
	if err := validateRequiredText("request id", "\xff", MaxRequestIDLength); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid UTF-8 error = %v, want ErrInvalidRequest", err)
	}
	if err := validateOptionalText("description", "", MaxDescriptionLength); err != nil {
		t.Fatalf("empty optional text: %v", err)
	}
}
