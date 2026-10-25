package core

import (
	"strings"
	"unicode"
)

// normalizeEmail is an email lowercased and trimmed, or 422 when it can't
// be an address.
func normalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	local, domain, ok := strings.Cut(email, "@")
	if !ok || len(email) > 254 || local == "" || !strings.Contains(domain, ".") || strings.IndexFunc(email, unicode.IsSpace) >= 0 {
		return "", unprocessable("email is not a valid address")
	}
	return email, nil
}
