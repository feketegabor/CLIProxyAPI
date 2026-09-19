package util

import (
	"net/http"
	"testing"
)

// A ChatGPT/Codex client sends session-id but never x-opencode-session; the
// custom header template must derive the session so Console Go accepts it.
func TestApplyCustomHeadersFromAttrs_DerivesOpencodeSessionFromSessionID(t *testing.T) {
	req, _ := http.NewRequest("POST", "http://upstream/v1/responses", nil)
	attrs := map[string]string{"header:X-Opencode-Session": "$x-opencode-session"}
	client := http.Header{}
	client.Set("Session-Id", "sess_abc123")
	ApplyCustomHeadersFromAttrs(req, attrs, client)
	if got := req.Header.Get("X-Opencode-Session"); got != "sess_abc123" {
		t.Fatalf("x-opencode-session = %q, want %q", got, "sess_abc123")
	}
}

func TestApplyCustomHeadersFromAttrs_ThreadIDFallback(t *testing.T) {
	req, _ := http.NewRequest("POST", "http://upstream/v1/responses", nil)
	attrs := map[string]string{"header:X-Opencode-Session": "$x-opencode-session"}
	client := http.Header{}
	client.Set("Thread-Id", "thread_77")
	ApplyCustomHeadersFromAttrs(req, attrs, client)
	if got := req.Header.Get("X-Opencode-Session"); got != "thread_77" {
		t.Fatalf("x-opencode-session = %q, want %q", got, "thread_77")
	}
}

// An explicit x-opencode-session must always win.
func TestApplyCustomHeadersFromAttrs_ExplicitSessionWins(t *testing.T) {
	req, _ := http.NewRequest("POST", "http://upstream/v1/responses", nil)
	attrs := map[string]string{"header:X-Opencode-Session": "$x-opencode-session"}
	client := http.Header{}
	client.Set("X-Opencode-Session", "opencode-native")
	client.Set("Session-Id", "sess_other")
	ApplyCustomHeadersFromAttrs(req, attrs, client)
	if got := req.Header.Get("X-Opencode-Session"); got != "opencode-native" {
		t.Fatalf("x-opencode-session = %q, want %q", got, "opencode-native")
	}
}

// With no identity headers at all, the header must be omitted (upstream keeps
// rejecting, same as the gateway's behavior).
func TestApplyCustomHeadersFromAttrs_NoIdentityOmits(t *testing.T) {
	req, _ := http.NewRequest("POST", "http://upstream/v1/responses", nil)
	attrs := map[string]string{"header:X-Opencode-Session": "$x-opencode-session"}
	ApplyCustomHeadersFromAttrs(req, attrs, http.Header{})
	if got := req.Header.Get("X-Opencode-Session"); got != "" {
		t.Fatalf("x-opencode-session should be omitted, got %q", got)
	}
}
