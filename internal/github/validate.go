package github

import (
	"errors"
	"net/url"
	"strings"
	"unicode"
)

func normalizeHost(raw string) (string, error) {
	if raw == "" {
		return defaultHost, nil
	}
	if raw != strings.TrimSpace(raw) || len(raw) > 253 {
		return "", errors.New("invalid GitHub host")
	}
	host := strings.ToLower(raw)
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("invalid GitHub host")
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return "", errors.New("invalid GitHub host")
			}
		}
	}
	return host, nil
}

func validateEndpoint(endpoint string) error {
	if endpoint == "" || endpoint != strings.TrimSpace(endpoint) || strings.HasPrefix(endpoint, "/") {
		return errors.New("invalid GitHub API endpoint")
	}
	for _, char := range endpoint {
		if unicode.IsControl(char) || char == '\\' {
			return errors.New("invalid GitHub API endpoint")
		}
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" || parsed.RawPath != "" {
		return errors.New("invalid GitHub API endpoint")
	}
	if strings.Contains(parsed.Path, "%") {
		return errors.New("invalid GitHub API endpoint")
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("invalid GitHub API endpoint")
		}
		for _, char := range segment {
			if !isEndpointChar(char) {
				return errors.New("invalid GitHub API endpoint")
			}
		}
	}
	if _, err := url.ParseQuery(parsed.RawQuery); err != nil {
		return errors.New("invalid GitHub API endpoint")
	}
	return nil
}

func isEndpointChar(char rune) bool {
	return char >= 'a' && char <= 'z' ||
		char >= 'A' && char <= 'Z' ||
		char >= '0' && char <= '9' ||
		strings.ContainsRune("-._~", char)
}

func validateQuery(query string) error {
	trimmed := strings.TrimSpace(query)
	if !strings.HasPrefix(trimmed, "query") || len(trimmed) == len("query") || isIdentifierChar(rune(trimmed[len("query")])) {
		return errors.New("invalid GraphQL query")
	}
	for _, token := range graphQLTokens(trimmed) {
		if token == "mutation" || token == "subscription" {
			return errors.New("GraphQL operation must be read-only")
		}
	}
	return nil
}

func graphQLTokens(query string) []string {
	var tokens []string
	for index := 0; index < len(query); {
		switch {
		case query[index] == '#':
			for index < len(query) && query[index] != '\n' && query[index] != '\r' {
				index++
			}
		case strings.HasPrefix(query[index:], `"""`):
			index += 3
			for index < len(query) && !strings.HasPrefix(query[index:], `"""`) {
				index++
			}
			if index < len(query) {
				index += 3
			}
		case query[index] == '"':
			index++
			for index < len(query) {
				if query[index] == '\\' {
					index += 2
					continue
				}
				if query[index] == '"' {
					index++
					break
				}
				index++
			}
		case isIdentifierChar(rune(query[index])):
			start := index
			for index < len(query) && isIdentifierChar(rune(query[index])) {
				index++
			}
			tokens = append(tokens, query[start:index])
		default:
			index++
		}
	}
	return tokens
}

func isIdentifierChar(char rune) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_'
}
