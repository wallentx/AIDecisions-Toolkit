package decisions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDecideRequestAndRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/decisions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var payload Request
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		var input map[string]string
		if err := json.Unmarshal([]byte(payload.Input), &input); err != nil {
			t.Error(err)
		}
		if payload.Model != DefaultModel || input["diff"] != "+ changed" || input["context"] != "public API" || len(payload.Questions) != 2 {
			t.Errorf("unexpected payload: %+v", payload)
		}
		io.WriteString(w, `{"answers":[{"name":"compatibility","type":"predicate","probability":0.9},{"name":"security","type":"refusal"}]}`)
	}))
	defer server.Close()
	client := Client{APIKey: "test-key", Endpoint: server.URL + "/decisions"}
	answers, err := client.Decide(context.Background(), testRequest(t, Checks{"compatibility": "Breaking?", "security": "Sensitive?"}))
	if err != nil {
		t.Fatal(err)
	}
	if *answers["compatibility"].Probability != 0.9 || answers["security"].Type != "refusal" || answers["security"].Probability != nil {
		t.Fatalf("answers lost information: %+v", answers)
	}
}

func TestRejectInvalidResponses(t *testing.T) {
	cases := map[string]string{
		"missing":             `{"answers":[]}`,
		"duplicate":           `{"answers":[{"name":"check","type":"refusal"},{"name":"check","type":"refusal"}]}`,
		"unknown":             `{"answers":[{"name":"other","type":"refusal"}]}`,
		"missing probability": `{"answers":[{"name":"check","type":"predicate"}]}`,
		"probability range":   `{"answers":[{"name":"check","type":"predicate","probability":1.1}]}`,
		"boolean probability": `{"answers":[{"name":"check","type":"predicate","probability":true}]}`,
		"wrong type":          `{"answers":[{"name":"check","type":"choice"}]}`,
		"invalid JSON":        `<html>error</html>`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
			defer server.Close()
			_, err := (Client{APIKey: "test-key", Endpoint: server.URL}).Decide(context.Background(), testRequest(t, Checks{"check": "Changed?"}))
			if err == nil {
				t.Fatal("invalid response was accepted")
			}
		})
	}
}

func TestHTTPFailureDoesNotEchoBodyOrKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "secret-key-in-error-body")
	}))
	defer server.Close()
	_, err := (Client{APIKey: "secret-key-in-error-body", Endpoint: server.URL}).Decide(context.Background(), testRequest(t, Checks{"check": "Changed?"}))
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "secret-key") {
		t.Fatalf("unsafe or missing error: %v", err)
	}
}

func TestRedirectRejected(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	_, err := (Client{APIKey: "test-key", Endpoint: server.URL}).Decide(context.Background(), testRequest(t, Checks{"check": "Changed?"}))
	if err == nil || calls != 1 {
		t.Fatalf("redirect followed: calls=%d err=%v", calls, err)
	}
}

func TestCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	_, err := (Client{APIKey: "test-key"}).Decide(ctx, testRequest(t, Checks{"check": "Changed?"}))
	if err == nil {
		t.Fatal("expired context accepted")
	}
}

func TestBuildRequestValidation(t *testing.T) {
	for _, checks := range []Checks{nil, {}, {"": "question"}, {"check": " "}} {
		if _, err := BuildRequest("+ x", "", checks); err == nil {
			t.Fatalf("accepted checks: %#v", checks)
		}
	}
	if _, err := BuildRequest(" \n", "", nil); err == nil {
		t.Fatal("accepted empty diff")
	}
}

func TestInvalidKeyIsNotEchoed(t *testing.T) {
	_, err := (Client{APIKey: "secret\nkey"}).Decide(context.Background(), testRequest(t, Checks{"check": "Changed?"}))
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("invalid key was not rejected safely: %v", err)
	}
}

func testRequest(t *testing.T, checks Checks) Request {
	t.Helper()
	request, err := BuildRequest(`{"diff":"+ changed","context":"public API"}`, "", checks)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestConfiguredModel(t *testing.T) {
	request, err := BuildRequest("evidence", "configured-model", Checks{"check": "Changed?"})
	if err != nil || request.Model != "configured-model" {
		t.Fatalf("configured model lost: %#v, %v", request, err)
	}
}
