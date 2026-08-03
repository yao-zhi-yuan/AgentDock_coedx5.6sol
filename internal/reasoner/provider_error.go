package reasoner

import (
	"context"
	"errors"
	"strings"
)

type statusCoder interface {
	StatusCode() int
}

type httpStatusCoder interface {
	HTTPStatus() int
}

func ClassifyProviderError(err error) StreamError {
	if errors.Is(err, context.Canceled) {
		return StreamError{Class: ErrorCanceled, Message: "reasoner request was canceled", Retryable: false}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return StreamError{Class: ErrorProviderUnavailable, Message: "provider request deadline exceeded", Retryable: true}
	}
	status := 0
	var statusErr statusCoder
	if errors.As(err, &statusErr) {
		status = statusErr.StatusCode()
	}
	var httpErr httpStatusCoder
	if status == 0 && errors.As(err, &httpErr) {
		status = httpErr.HTTPStatus()
	}
	switch {
	case status == 401 || status == 403:
		return StreamError{Class: ErrorProviderAuthentication, Message: "provider authentication failed", Retryable: false}
	case status == 429:
		return StreamError{Class: ErrorProviderRateLimited, Message: "provider rate limit exceeded", Retryable: true}
	case status == 408 || status >= 500:
		return StreamError{Class: ErrorProviderUnavailable, Message: "provider is temporarily unavailable", Retryable: true}
	case status >= 400:
		return StreamError{Class: ErrorProviderInvalidRequest, Message: "provider rejected the request", Retryable: false}
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "rate limit") || strings.Contains(message, "too many requests"):
		return StreamError{Class: ErrorProviderRateLimited, Message: "provider rate limit exceeded", Retryable: true}
	case strings.Contains(message, "unauthorized") || strings.Contains(message, "authentication") || strings.Contains(message, "invalid api key"):
		return StreamError{Class: ErrorProviderAuthentication, Message: "provider authentication failed", Retryable: false}
	case strings.Contains(message, "invalid request") || strings.Contains(message, "bad request"):
		return StreamError{Class: ErrorProviderInvalidRequest, Message: "provider rejected the request", Retryable: false}
	default:
		return StreamError{Class: ErrorProviderUnavailable, Message: "provider is temporarily unavailable", Retryable: true}
	}
}

// ClassifyProviderStreamError preserves known provider classes after Stream
// setup while treating an unclassified transport break as a recoverable
// interruption. It never copies provider text into the normalized message.
func ClassifyProviderStreamError(err error) StreamError {
	failure := ClassifyProviderError(err)
	if failure.Class == ErrorProviderUnavailable &&
		!errors.Is(err, context.DeadlineExceeded) && providerStatus(err) == 0 {
		return StreamError{Class: ErrorStreamingInterrupted, Message: "model stream was interrupted", Retryable: true}
	}
	return failure
}

func providerStatus(err error) int {
	var statusErr statusCoder
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode()
	}
	var httpErr httpStatusCoder
	if errors.As(err, &httpErr) {
		return httpErr.HTTPStatus()
	}
	return 0
}
