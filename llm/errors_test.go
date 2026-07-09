package llm

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsEncryptedReplayRejection(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "invalid encrypted content",
			err:  &ProviderError{StatusCode: 400, Err: errors.New("INVALID_ENCRYPTED_CONTENT: rs_123")},
			want: true,
		},
		{
			name: "decryption and parsing message",
			err:  &ProviderError{Err: errors.New("Encrypted content could not be decrypted or parsed: rs_123")},
			want: true,
		},
		{
			name: "wrapped stream payload",
			err:  fmt.Errorf("stream failed: %w", &ProviderError{StatusCode: 400, Err: errors.New(`received error while streaming: {"code":400,"message":"could not be decrypted or parsed"}`)}),
			want: true,
		},
		{
			name: "context length",
			err:  &ProviderError{StatusCode: 400, Err: errors.New("maximum context length exceeded")},
			want: false,
		},
		{
			name: "generic bad request",
			err:  &ProviderError{StatusCode: 400, Err: errors.New("invalid request")},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsEncryptedReplayRejection(tc.err); got != tc.want {
				t.Errorf("IsEncryptedReplayRejection(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
