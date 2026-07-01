package money

import (
	"errors"
	"testing"
)

func TestNewNormalisesCurrency(t *testing.T) {
	m := New(1099, " usd ")
	if m.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", m.Currency)
	}
	if m.Amount != 1099 {
		t.Fatalf("amount = %d, want 1099", m.Amount)
	}
}

func TestAdd(t *testing.T) {
	a := New(1000, "USD")
	b := New(250, "USD")
	got, err := a.Add(b)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Amount != 1250 {
		t.Fatalf("sum = %d, want 1250", got.Amount)
	}
}

func TestAddCurrencyMismatch(t *testing.T) {
	_, err := New(1000, "USD").Add(New(1, "EUR"))
	if !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("err = %v, want ErrCurrencyMismatch", err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		m       Money
		wantErr bool
	}{
		{"ok", New(100, "USD"), false},
		{"zero", New(0, "USD"), true},
		{"negative", New(-5, "USD"), true},
		{"bad currency", New(100, "US"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.m.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestString(t *testing.T) {
	tests := map[string]Money{
		"10.99 USD":  New(1099, "USD"),
		"0.05 EUR":   New(5, "EUR"),
		"-3.00 GBP":  New(-300, "GBP"),
		"100.00 USD": New(10000, "USD"),
	}
	for want, m := range tests {
		if got := m.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}
