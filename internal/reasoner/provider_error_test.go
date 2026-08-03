package reasoner_test

import (
	"errors"
	"testing"

	"github.com/agentdock/agentdock-verify/internal/reasoner"
)

type providerStatusError struct {
	status int
}

func (providerStatusError) Error() string       { return "provider error" }
func (err providerStatusError) StatusCode() int { return err.status }

func TestProviderErrorsAreClassifiedWithoutCopyingRawDetails(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		class     reasoner.ErrorClass
		retryable bool
	}{
		{name: "authentication", err: providerStatusError{status: 401}, class: reasoner.ErrorProviderAuthentication},
		{name: "rate-limit", err: providerStatusError{status: 429}, class: reasoner.ErrorProviderRateLimited, retryable: true},
		{name: "invalid", err: providerStatusError{status: 400}, class: reasoner.ErrorProviderInvalidRequest},
		{name: "unavailable", err: providerStatusError{status: 503}, class: reasoner.ErrorProviderUnavailable, retryable: true},
		{name: "fallback", err: errors.New("connection reset with credential sk-do-not-copy"), class: reasoner.ErrorProviderUnavailable, retryable: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure := reasoner.ClassifyProviderError(test.err)
			if failure.Class != test.class || failure.Retryable != test.retryable {
				t.Fatalf("ClassifyProviderError() = %#v", failure)
			}
			if failure.Message == test.err.Error() {
				t.Fatal("classified error copied raw provider details")
			}
		})
	}
}
