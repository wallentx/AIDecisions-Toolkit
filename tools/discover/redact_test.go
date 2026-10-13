package discover

import (
	"strings"
	"testing"
)

func TestRedactSensitiveForms(t *testing.T) {
	cases := []struct{ name, input, private string }{
		{"provider token", "use sk-ant-abcdefghijklmnopqrstuvwxyz123456", "sk-ant-abcdefghijklmnopqrstuvwxyz123456"},
		{"authorization", "Authorization: Bearer secret-value\nContinue calibration", "secret-value"},
		{"url credentials", "https://person:password@example.invalid/path?api_key=hidden", "password"},
		{"email", "contact person@example.invalid today", "person@example.invalid"},
		{"natural password", "my password is swordfish123", "swordfish123"},
		{"pairing", "pairing code: 123456", "123456"},
		{"quoted secret", `{"password":"correct horse battery staple"}`, "correct horse battery staple"},
		{"home path", "read /data/data/com.termux/files/home/person/private.txt", "/data/data/com.termux/files/home/person/private.txt"},
		{"windows path", `read C:\Users\person\private.txt`, `C:\Users\person\private.txt`},
		{"opaque", "value AbC9dE2fG7hI4jK8LmN0pQrStU5vWxYz", "AbC9dE2fG7hI4jK8LmN0pQrStU5vWxYz"},
		{"private key", "-----BEGIN OPENSSH PRIVATE KEY-----\nprivate-key-material\n-----END OPENSSH PRIVATE KEY-----", "private-key-material"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			output := Redact(test.input)
			if strings.Contains(output, test.private) {
				t.Fatalf("sensitive value retained: %s", test.name)
			}
		})
	}
}

func TestRedactControlsAndPreserveUsefulText(t *testing.T) {
	input := "Inspect\x1b[31m orchard\x1b[0m\x00 calibration\u202e records."
	output := Redact(input)
	if output != "Inspect orchard calibration records." {
		t.Fatalf("unexpected sanitized text: %q", output)
	}
}

func TestNaturalMultiwordPassphrase(t *testing.T) {
	output := Redact("my passphrase is correct horse battery staple\nInspect orchard calibration.")
	if strings.Contains(output, "horse") || !strings.Contains(output, "orchard") {
		t.Fatal("multiword passphrase not redacted safely")
	}
	output = Redact("path-AbC9dE2fG7hI4jK8LmN0pQrStU5vWxYz")
	if strings.Contains(output, "AbC9dE2f") {
		t.Fatal("opaque prefix bypass")
	}
}

func TestAbsoluteRepositoryPath(t *testing.T) {
	output := Redact("Inspect /srv/private-owner/private-repo/src/main.go for calibration failures.")
	if strings.Contains(output, "private-owner") || strings.Contains(output, "private-repo") {
		t.Fatal("absolute repository path leaked")
	}
}

func TestWindowsAbsolutePaths(t *testing.T) {
	for _, path := range []string{`D:\work\private\repo`, `C:\ProgramData\secret`, `\\server\share\team\file`, `D:/work/private/repo`} {
		t.Run(path, func(t *testing.T) {
			input := "Inspect " + path + " for calibration failures."
			output := Redact(input)
			if output != "Inspect [path] for calibration failures." {
				t.Fatalf("absolute Windows path was not fully redacted: %q", output)
			}
			first := redact(input, []byte("synthetic-installation-salt"))
			second := redact(input, []byte("synthetic-installation-salt"))
			if first != second || strings.Contains(first, path) || !strings.Contains(first, "[path-") {
				t.Fatal("Windows path alias is missing or unstable")
			}
		})
	}
}
