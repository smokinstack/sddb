package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Default models, overridable with ANTHROPIC_MODEL / OPENAI_MODEL /
// OLLAMA_MODEL so a retired model name doesn't need a code change.
const (
	DefaultAnthropicModel = "claude-sonnet-5-5"
	DefaultOpenAIModel    = "gpt-4o"
	DefaultOllamaModel    = "llama3.2"
)

// Config holds the provider credentials and models (from the environment).
type Config struct {
	AnthropicKey   string
	AnthropicModel string
	OpenAIKey      string
	OpenAIModel    string
	OllamaURL      string // e.g. http://192.168.1.10:11434
	OllamaModel    string
}

// Client calls Claude, OpenAI, or a local Ollama instance depending on what is configured.
// Priority: Claude > OpenAI > Ollama.
type Client struct {
	cfg  Config
	http *http.Client

	// API endpoints; overridable in tests.
	anthropicURL string
	openaiURL    string
}

func New(cfg Config) *Client {
	if cfg.AnthropicModel == "" {
		cfg.AnthropicModel = DefaultAnthropicModel
	}
	if cfg.OpenAIModel == "" {
		cfg.OpenAIModel = DefaultOpenAIModel
	}
	if cfg.OllamaURL != "" && cfg.OllamaModel == "" {
		cfg.OllamaModel = DefaultOllamaModel
	}
	return &Client{
		cfg:          cfg,
		http:         &http.Client{Timeout: 5 * time.Minute},
		anthropicURL: "https://api.anthropic.com/v1/messages",
		openaiURL:    "https://api.openai.com/v1/chat/completions",
	}
}

func (c *Client) Available() bool {
	return c.cfg.AnthropicKey != "" || c.cfg.OpenAIKey != "" || c.cfg.OllamaURL != ""
}

// Provider returns a human-readable description of the default provider.
func (c *Client) Provider() string {
	if p := c.resolve(""); p != "" {
		return c.label(p)
	}
	return "none"
}

// AvailableProviders returns the names of configured providers.
func (c *Client) AvailableProviders() []string {
	var out []string
	if c.cfg.AnthropicKey != "" {
		out = append(out, "claude")
	}
	if c.cfg.OpenAIKey != "" {
		out = append(out, "openai")
	}
	if c.cfg.OllamaURL != "" {
		out = append(out, "ollama")
	}
	return out
}

// resolve returns the provider that will handle a request: the preferred one
// if it's configured, otherwise the first configured in priority order.
func (c *Client) resolve(preferred string) string {
	for _, p := range c.AvailableProviders() {
		if p == preferred {
			return p
		}
	}
	if ps := c.AvailableProviders(); len(ps) > 0 {
		return ps[0]
	}
	return ""
}

// label describes a provider and model, e.g. "Claude (claude-sonnet-5-5)".
// It's shown next to each result, so it's clear where the data was sent.
func (c *Client) label(p string) string {
	switch p {
	case "claude":
		return fmt.Sprintf("Claude (%s)", c.cfg.AnthropicModel)
	case "openai":
		return fmt.Sprintf("OpenAI (%s)", c.cfg.OpenAIModel)
	case "ollama":
		return fmt.Sprintf("Ollama (%s at %s)", c.cfg.OllamaModel, c.cfg.OllamaURL)
	}
	return "none"
}

// AskWithProvider sends prompt to the preferred provider (falling back to the
// first configured one) and returns the answer and a label for the provider
// that was actually used.
func (c *Client) AskWithProvider(ctx context.Context, prompt, preferred string) (answer, usedProvider string, err error) {
	p := c.resolve(preferred)
	switch p {
	case "claude":
		answer, err = c.askClaude(ctx, prompt)
	case "openai":
		answer, err = c.askChat(ctx, "openai", c.openaiURL, c.cfg.OpenAIKey, c.cfg.OpenAIModel, prompt)
	case "ollama":
		answer, err = c.askChat(ctx, "ollama", strings.TrimRight(c.cfg.OllamaURL, "/")+"/v1/chat/completions", "", c.cfg.OllamaModel, prompt)
	default:
		return "", "", fmt.Errorf("no AI provider configured")
	}
	return answer, c.label(p), err
}

func (c *Client) askClaude(ctx context.Context, prompt string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      c.cfg.AnthropicModel,
		"max_tokens": 2048,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.anthropicURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", c.cfg.AnthropicKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := c.do(req, "claude", &out); err != nil {
		return "", err
	}
	if len(out.Content) == 0 {
		return "", fmt.Errorf("claude: empty response")
	}
	return out.Content[0].Text, nil
}

// askChat calls an OpenAI-compatible chat completions endpoint (OpenAI, Ollama).
func (c *Client) askChat(ctx context.Context, name, url, key, model, prompt string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
		"stream":   false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("content-type", "application/json")

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := c.do(req, name, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("%s: empty response", name)
	}
	return out.Choices[0].Message.Content, nil
}

// do sends req and decodes a successful JSON response into out. Failures
// report the HTTP status and the provider's error message, or the start of
// the body when it isn't JSON (e.g. an HTML error page from a proxy), rather
// than a bare JSON decode error.
func (c *Client) do(req *http.Request, name string, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("%s: read response: %w", name, err)
	}

	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := ""
		if json.Unmarshal(body, &e) == nil {
			msg = e.Error.Message
		}
		if msg == "" {
			msg = strings.TrimSpace(string(body))
			if len(msg) > 200 {
				msg = msg[:200] + "…"
			}
		}
		return fmt.Errorf("%s: HTTP %d: %s", name, resp.StatusCode, msg)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: unexpected response: %w", name, err)
	}
	return nil
}
