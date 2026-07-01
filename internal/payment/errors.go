package payment

import "fmt"

// ErrorCode categorizes gateway failures, mirroring Stripe's error taxonomy.
type ErrorCode string

const (
	CodeCard      ErrorCode = "card_error"
	CodeRateLimit ErrorCode = "rate_limit"
	CodeAPI       ErrorCode = "api_error"
	CodeInvalid   ErrorCode = "invalid_request"
	CodeNotFound  ErrorCode = "not_found"
)

// Error is a typed gateway error carrying a retryability hint.
type Error struct {
	Code      ErrorCode
	Message   string
	Retryable bool
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// NewError builds a gateway Error.
func NewError(code ErrorCode, msg string) *Error {
	return &Error{Code: code, Message: msg, Retryable: code == CodeRateLimit || code == CodeAPI}
}
