package user

import "strings"

// NormalizeName should ignore surrounding whitespace before normalizing case.
func NormalizeName(name string) string {
	return strings.ToLower(name)
}
