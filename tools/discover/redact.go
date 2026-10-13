package discover

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"regexp"
	"strings"
	"unicode"
)

var (
	privateKeyPattern    = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`)
	ansiPattern          = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|$))`)
	emailPattern         = regexp.MustCompile(`(?i)[a-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-z0-9-]+(?:\.[a-z0-9-]+)+`)
	tokenPattern         = regexp.MustCompile(`(?i)\b(?:sk-[a-z0-9_-]{12,}|gh[pousr]_[a-z0-9]{12,}|github_pat_[a-z0-9_]{12,}|AKIA[A-Z0-9]{16}|xox[baprs]-[a-z0-9-]{10,}|eyJ[a-z0-9_-]{8,}\.[a-z0-9_-]+\.[a-z0-9_-]+)`)
	authorizationPattern = regexp.MustCompile(`(?im)\b(?:authorization|proxy-authorization)\s*:\s*[^\r\n]+`)
	bearerPattern        = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[a-z0-9+/_.=-]{8,}`)
	naturalSecretPattern = regexp.MustCompile(`(?i)\b(?:password|passwd|passphrase|secret|api[ _-]?key|pair(?:ing)?[ _-]?code|verification[ _-]?code)\s+is\s+[^\r\n]+`)
	assignmentPattern    = regexp.MustCompile(`(?i)\b(?:password|passwd|passphrase|secret|api[ _-]?key|access[ _-]?token|auth[ _-]?token|refresh[ _-]?token|pair(?:ing)?[ _-]?code|verification[ _-]?code|client[ _-]?secret|token)\b["']?\s*(?::|=|\bis\b)\s*(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;]+)`)
	urlPattern           = regexp.MustCompile(`(?i)https?://[^\s<>"'\x60]+`)
	githubPattern        = regexp.MustCompile(`(?i)^https?://github\.com/([^/?#]+)/([^/?#]+)/(pull|issues|actions/runs)/([0-9]+)`)
	pathPattern          = regexp.MustCompile(`(?:[A-Za-z]:\\(?:Users|home)\\[^\s"'<>]+|/(?:data/data/[^/\s]+/files/(?:home|usr)|home|Users|root|tmp|var/tmp|workspace|workspaces|private)/[^\s"'<>]+|~[/\\][^\s"'<>]+)`)
	windowsPathPattern   = regexp.MustCompile(`(?:[A-Za-z]:[/\\][^\s"'<>]+|\\\\[^\s\\/"'<>]+[/\\][^\s"'<>]+)`)
	aliasPattern         = regexp.MustCompile(`^(?:path|github-(?:pr|run|issue))-[0-9a-f]{24}$`)
	absolutePathPattern  = regexp.MustCompile(`/(?:[a-zA-Z0-9._-]+/)+[^\s"'<>]+`)
	opaquePattern        = regexp.MustCompile(`[A-Za-z0-9_+/=-]{24,}`)
)

func alias(salt []byte, category, value string) string {
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(category + "\x00" + value))
	return category + "-" + hex.EncodeToString(mac.Sum(nil)[:12])
}

// Redact removes recognizable sensitive values. It is best effort: callers
// must still screen excerpts before sharing them outside the current agent.
func Redact(text string) string { return redact(text, nil) }

func redact(text string, salt []byte) string {
	text = privateKeyPattern.ReplaceAllString(text, "[private-key]")
	text = ansiPattern.ReplaceAllString(text, "")
	text = strings.Map(func(r rune) rune {
		if (unicode.IsControl(r) && r != '\n' && r != '\t') || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, text)
	text = authorizationPattern.ReplaceAllString(text, "[authorization]")
	text = bearerPattern.ReplaceAllString(text, "[authorization]")
	text = naturalSecretPattern.ReplaceAllString(text, "[secret]")
	text = assignmentPattern.ReplaceAllString(text, "[secret]")
	text = tokenPattern.ReplaceAllString(text, "[token]")
	text = urlPattern.ReplaceAllStringFunc(text, func(value string) string {
		match := githubPattern.FindStringSubmatch(value)
		if len(match) != 0 && len(salt) != 0 {
			family := "github-issue"
			if strings.EqualFold(match[3], "pull") {
				family = "github-pr"
			}
			if strings.EqualFold(match[3], "actions/runs") {
				family = "github-run"
			}
			canonical := strings.ToLower(match[1]+"/"+match[2]+"/"+match[3]) + "/" + match[4]
			return "[" + alias(salt, family, canonical) + "]"
		}
		return "[url]"
	})
	text = emailPattern.ReplaceAllString(text, "[email]")
	redactPath := func(value string) string {
		if len(salt) == 0 {
			return "[path]"
		}
		return "[" + alias(salt, "path", value) + "]"
	}
	text = windowsPathPattern.ReplaceAllStringFunc(text, redactPath)
	text = pathPattern.ReplaceAllStringFunc(text, redactPath)
	text = absolutePathPattern.ReplaceAllStringFunc(text, redactPath)
	// This deliberately errs toward hiding opaque identifiers; generated aliases
	// are retained for stable resource and path references.
	text = opaquePattern.ReplaceAllStringFunc(text, func(value string) string {
		if aliasPattern.MatchString(value) {
			return value
		}
		counts := make(map[rune]int)
		for _, r := range value {
			counts[r]++
		}
		entropy := 0.0
		for _, n := range counts {
			p := float64(n) / float64(len(value))
			entropy -= p * math.Log2(p)
		}
		if entropy >= 3.5 {
			return "[opaque]"
		}
		return value
	})
	return text
}
