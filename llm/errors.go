package llm

import (
	"errors"
	"fmt"
	"strings"
)

// ErrorKind is a normalized error classification (decision 9). Adapters map
// provider/SDK errors into one of these so callers can react uniformly across
// providers (e.g. back off a rate-limited model in a comparison run).
type ErrorKind string

const (
	KindRateLimit     ErrorKind = "rate_limit"
	KindAuth          ErrorKind = "auth"
	KindContextLength ErrorKind = "context_length"
	KindContentFilter ErrorKind = "content_filter"
	KindCanceled      ErrorKind = "canceled"
	KindUnsupported   ErrorKind = "unsupported"
	KindUnknown       ErrorKind = "unknown"
)

// ProviderError wraps an underlying error with a normalized classification.
// The original error is preserved (Unwrap), so errors.Is/As reach the SDK's
// own error type and the Raw details remain accessible.
type ProviderError struct {
	Provider   string    // which provider produced it (e.g. "xai")
	Kind       ErrorKind // normalized classification
	StatusCode int       // HTTP status, if known (0 otherwise)
	Retryable  bool      // whether a retry might succeed
	Err        error     // the wrapped original error
}

func (e *ProviderError) Error() string {
	prefix := fmt.Sprintf("llm: %s: %s", e.Provider, e.Kind)
	if e.StatusCode != 0 {
		prefix = fmt.Sprintf("%s (status %d)", prefix, e.StatusCode)
	}
	if e.Err != nil {
		return prefix + ": " + e.Err.Error()
	}
	return prefix
}

// Unwrap exposes the wrapped error to errors.Is / errors.As.
func (e *ProviderError) Unwrap() error { return e.Err }

// IsEncryptedReplayRejection reports the deterministic 400 returned when a
// provider cannot re-verify an encrypted reasoning item from prior history.
// The provider's error envelopes vary, so matching intentionally uses the
// complete error text rather than a provider-specific error code.
func IsEncryptedReplayRejection(err error) bool {
	var pe *ProviderError
	if !errors.As(err, &pe) || (pe.StatusCode != 0 && pe.StatusCode != 400) {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "invalid_encrypted_content") ||
		strings.Contains(message, "encrypted content could not be decrypted") ||
		(strings.Contains(message, "could not be decrypted") &&
			strings.Contains(message, "parsed"))
}

// Is matches on Kind, so callers can write errors.Is(err, llm.ErrRateLimit)
// regardless of which provider or status code produced it.
func (e *ProviderError) Is(target error) bool {
	t, ok := target.(*ProviderError)
	return ok && t.Kind == e.Kind
}

// Sentinel values for errors.Is checks. They carry only a Kind.
var (
	ErrRateLimit     = &ProviderError{Kind: KindRateLimit}
	ErrAuth          = &ProviderError{Kind: KindAuth}
	ErrContextLength = &ProviderError{Kind: KindContextLength}
	ErrContentFilter = &ProviderError{Kind: KindContentFilter}
	ErrCanceled      = &ProviderError{Kind: KindCanceled}
	ErrUnsupported   = &ProviderError{Kind: KindUnsupported}
)
