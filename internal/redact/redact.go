package redact

import (
	"net/url"
	"regexp"
	"strings"
)

const replacement = "[REDACTED]"

var httpURLPattern = regexp.MustCompile(`https?://[^\s"'<>]+`)

func Secrets(value string, secrets ...string) string {
	for _, secret := range secrets {
		if strings.TrimSpace(secret) == "" {
			continue
		}
		value = strings.ReplaceAll(value, secret, replacement)
	}
	return value
}

func ContainsSecret(value string, secrets ...string) bool {
	for _, secret := range secrets {
		if strings.TrimSpace(secret) == "" {
			continue
		}
		if strings.Contains(value, secret) {
			return true
		}
	}
	return false
}

func URLHasCredentials(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil {
		return false
	}
	if parsed.User != nil {
		return true
	}
	for key := range parsed.Query() {
		if sensitiveQueryKey(key) {
			return true
		}
	}
	return false
}

func SafeURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return replacement
	}
	changed := false
	if parsed.User != nil {
		parsed.User = nil
		changed = true
	}
	query := parsed.Query()
	for key := range query {
		if sensitiveQueryKey(key) {
			query.Del(key)
			changed = true
		}
	}
	if changed {
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	return value
}

func SafeText(value string) string {
	return httpURLPattern.ReplaceAllStringFunc(value, func(candidate string) string {
		trailing := ""
		for len(candidate) > 0 {
			last := candidate[len(candidate)-1]
			if last != '.' && last != ',' && last != ';' && last != ')' && last != ']' && last != '}' {
				break
			}
			trailing = string(last) + trailing
			candidate = candidate[:len(candidate)-1]
		}
		if URLHasCredentials(candidate) {
			return SafeURL(candidate) + trailing
		}
		return candidate + trailing
	})
}

func sensitiveQueryKey(key string) bool {
	key = strings.ToLower(key)
	return strings.Contains(key, "token") ||
		strings.Contains(key, "password") ||
		strings.Contains(key, "secret") ||
		strings.Contains(key, "credential") ||
		key == "auth" ||
		key == "authorization"
}
