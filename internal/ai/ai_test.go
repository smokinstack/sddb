package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModelDefaultsAndOverrides(t *testing.T) {
	c := New(Config{AnthropicKey: "k"})
	if got := c.Provider(); got != "Claude (claude-sonnet-5-5)" {
		t.Errorf("default = %q", got)
	}
	c = New(Config{AnthropicKey: "k", AnthropicModel: "claude-opus-5-5"})
	if got := c.Provider(); got != "Claude (claude-opus-5-5)" {
		t.Errorf("override = %q", got)
	}
}

func TestProviderSelection(t *testing.T) {
	c := New(Config{OpenAIKey: "o", OllamaURL: "http://ollama:11434"})
	if got := c.resolve("claude"); got != "openai" {
		t.Errorf("unconfigured preference should fall back to the first configured provider, got %q", got)
	}
	if got := c.resolve("ollama"); got != "ollama" {
		t.Errorf("configured preference ignored, got %q", got)
	}
}

func TestClaudeRequestAndProviderLabel(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Model string }
		json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		w.Write([]byte(`{"content":[{"text":"all good"}]}`))
	}))
	defer srv.Close()
	c := New(Config{AnthropicKey: "k", AnthropicModel: "claude-haiku-5-5"})
	c.anthropicURL = srv.URL

	answer, used, err := c.AskWithProvider(context.Background(), "hi", "")
	if err != nil || answer != "all good" || used != "Claude (claude-haiku-5-5)" || gotModel != "claude-haiku-5-5" {
		t.Fatalf("answer=%q used=%q model=%q err=%v", answer, used, gotModel, err)
	}
}

func TestErrorsShowStatusAndMessage(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		want       string
	}{
		{"JSON error", `{"error":{"message":"model not found"}}`, 404, "HTTP 404: model not found"},
		{"HTML error page from a proxy", "<html><body>502 Bad Gateway</body></html>", 502, "HTTP 502: <html><body>502 Bad Gateway"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := New(Config{OpenAIKey: "k"})
			c.openaiURL = srv.URL
			_, used, err := c.AskWithProvider(context.Background(), "hi", "openai")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if used != "OpenAI (gpt-4o)" {
				t.Errorf("used = %q", used)
			}
		})
	}
}
