// Package money models monetary values as integer minor units (e.g. cents)
// plus an ISO-4217 currency code. Amounts are never represented as floats.
package money

import (
	"errors"
	"fmt"
	"strings"
)

// Money is an amount in the smallest unit of its currency.
type Money struct {
	Amount   int64  // minor units (e.g. 1099 == 10.99)
	Currency string // ISO-4217, upper-case (e.g. "USD")
}

// ErrCurrencyMismatch is returned when arithmetic mixes currencies.
var ErrCurrencyMismatch = errors.New("money: currency mismatch")

// New builds a Money, normalising the currency code.
func New(amount int64, currency string) Money {
	return Money{Amount: amount, Currency: strings.ToUpper(strings.TrimSpace(currency))}
}

// Zero returns a zero amount in the given currency.
func Zero(currency string) Money { return New(0, currency) }

// IsZero reports whether the amount is zero.
func (m Money) IsZero() bool { return m.Amount == 0 }

// IsPositive reports whether the amount is greater than zero.
func (m Money) IsPositive() bool { return m.Amount > 0 }

// Add returns the sum, requiring matching currencies.
func (m Money) Add(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, ErrCurrencyMismatch
	}
	return Money{Amount: m.Amount + o.Amount, Currency: m.Currency}, nil
}

// Validate checks the value is usable as a charge amount.
func (m Money) Validate() error {
	if len(m.Currency) != 3 {
		return fmt.Errorf("money: invalid currency %q", m.Currency)
	}
	if m.Amount <= 0 {
		return errors.New("money: amount must be positive")
	}
	return nil
}

// String renders a two-decimal representation, e.g. "10.99 USD".
func (m Money) String() string {
	sign := ""
	a := m.Amount
	if a < 0 {
		sign, a = "-", -a
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, a/100, a%100, m.Currency)
}
