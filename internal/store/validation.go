package store

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strings"

	"github.com/agentdock/agentdock-verify/internal/domain"
)

var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bsk-(?:proj-|svcacct-)?[a-z0-9_-]{16,}\b`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`(?i)\b(?:aws_secret_access_key|service_token|api_key|access_token|refresh_token|client_secret|database_url)\s*=\s*\S+`),
	regexp.MustCompile(`(?i)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`),
}

const maxSensitiveJSONDepth = 32

func validateAppendInput(event domain.Event) error {
	if event.RunID == "" || event.IdempotencyKey == "" || event.Seq != 0 {
		return fmt.Errorf("%w: run_id and idempotency_key are required and seq must be zero", ErrInvalidAppend)
	}
	if err := ValidateEventData(event.Data); err != nil {
		return fmt.Errorf("%w: run_id=%q idempotency_key=%q", ErrSensitivePayload, event.RunID, event.IdempotencyKey)
	}
	return nil
}

// ValidateEventData applies the same credential-material rejection to durable
// Receipt output before it can later become ActionCompleted payload.
func ValidateEventData(data domain.EventData) error {
	if containsCredentialMaterial(data) {
		return ErrSensitivePayload
	}
	return nil
}

func containsCredentialMaterial(data domain.EventData) bool {
	value := reflect.ValueOf(data)
	for index := 0; index < value.NumField(); index++ {
		field := value.Field(index)
		if field.Kind() != reflect.String {
			continue
		}
		if hasSensitiveString(field.String(), 0) {
			return true
		}
	}
	return false
}

func hasSensitiveJSONAtDepth(value any, depth int) bool {
	if depth >= maxSensitiveJSONDepth {
		return true
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if sensitiveKey(key) || hasSensitiveJSONAtDepth(child, depth+1) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if hasSensitiveJSONAtDepth(child, depth+1) {
				return true
			}
		}
	case string:
		return hasSensitiveString(typed, depth+1)
	}
	return false
}

func hasSensitiveString(value string, depth int) bool {
	if hasSensitiveValue(value) {
		return true
	}
	if depth >= maxSensitiveJSONDepth {
		return true
	}
	var decoded any
	return json.Unmarshal([]byte(value), &decoded) == nil &&
		hasSensitiveJSONAtDepth(decoded, depth+1)
}

func hasSensitiveValue(value string) bool {
	if hasSensitiveMarker(value) || hasCredentialURL(value) {
		return true
	}
	for _, pattern := range credentialPatterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	return false
}

func sensitiveKey(key string) bool {
	normalized := normalizeSensitiveKey(key)
	switch normalized {
	case "api_key", "apikey", "password", "passwd", "credential", "credentials",
		"authorization", "access_token", "refresh_token", "private_key",
		"database_url", "secret", "client_secret", "token", "env", "environment",
		"openai_api_key", "anthropic_api_key", "aws_secret_access_key", "service_token":
		return true
	default:
		return strings.HasSuffix(normalized, "_api_key") ||
			strings.HasSuffix(normalized, "_password") ||
			strings.HasSuffix(normalized, "_credential") ||
			strings.HasSuffix(normalized, "_secret")
	}
}

func normalizeSensitiveKey(key string) string {
	var normalized strings.Builder
	lastWasSeparator := false
	for _, character := range strings.ToLower(key) {
		if (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') {
			normalized.WriteRune(character)
			lastWasSeparator = false
			continue
		}
		if normalized.Len() > 0 && !lastWasSeparator {
			normalized.WriteByte('_')
			lastWasSeparator = true
		}
	}
	return strings.Trim(normalized.String(), "_")
}

func hasCredentialURL(value string) bool {
	for _, field := range strings.Fields(value) {
		candidate := strings.Trim(field, `"'(),;`)
		parsed, err := url.Parse(candidate)
		if err == nil && parsed.Scheme != "" && parsed.User != nil {
			if _, hasPassword := parsed.User.Password(); hasPassword {
				return true
			}
		}
	}
	return false
}

func hasSensitiveMarker(value string) bool {
	normalized := strings.ToLower(value)
	for _, marker := range []string{
		"openai_api_key=",
		"anthropic_api_key=",
		"database_url=postgres://",
		"authorization: bearer ",
		`"api_key":`,
		`"password":`,
		`"access_token":`,
		`"refresh_token":`,
		`"private_key":`,
		`"client_secret":`,
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}
