package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client calls Claude, OpenAI, or a local Ollama instance depending on what is configured.
// Priority: Claude > OpenAI > Ollama.
type Client struct {
	anthropicKey string
	openaiKey    string
	ollamaURL    string // e.g. http://192.168.1.10:11434
	ollamaModel  string // e.g. llama3.2
	http         *http.Client
}

func New(anthropicKey, openaiKey, ollamaURL, ollamaModel string) *Client {
	if ollamaURL != "" && ollamaModel == "" {
		ollamaModel = "llama3.2"
	}
	return &Client{
		anthropicKey: anthropicKey,
		openaiKey:    openaiKey,
		ollamaURL:    ollamaURL,
		ollamaModel:  ollamaModel,
		http:         &http.Client{Timeout: 5 * time.Minute},
	}
}

func (c *Client) Available() bool {
	return c.anthropicKey != "" || c.openaiKey != "" || c.ollamaURL != ""
}

// Provider returns a human-readable description of the active provider.
func (c *Client) Provider() string {
	switch {
	case c.anthropicKey != "":
		return "Claude (Anthropic)"
	case c.openaiKey != "":
		return "OpenAI"
	case c.ollamaURL != "":
		return fmt.Sprintf("Ollama model=%s url=%s", c.ollamaModel, c.ollamaURL)
	default:
		return "none"
	}
}

// AvailableProviders returns the names of configured providers.
func (c *Client) AvailableProviders() []string {
	var out []string
	if c.anthropicKey != "" {
		out = append(out, "claude")
	}
	if c.openaiKey != "" {
		out = append(out, "openai")
	}
	if c.ollamaURL != "" {
		out = append(out, "ollama")
	}
	return out
}

func (c *Client) Ask(ctx context.Context, prompt string) (string, error) {
	switch {
	case c.anthropicKey != "":
		return c.askClaude(ctx, prompt)
	case c.openaiKey != "":
		return c.askOpenAI(ctx, prompt)
	default:
		return c.askOllama(ctx, prompt)
	}
}

// AskWithProvider uses a specific provider, falling back to Ask if provider is empty or unconfigured.
func (c *Client) AskWithProvider(ctx context.Context, prompt, provider string) (string, error) {
	switch provider {
	case "claude":
		if c.anthropicKey != "" {
			return c.askClaude(ctx, prompt)
		}
	case "openai":
		if c.openaiKey != "" {
			return c.askOpenAI(ctx, prompt)
		}
	case "ollama":
		if c.ollamaURL != "" {
			return c.askOllama(ctx, prompt)
		}
	}
	return c.Ask(ctx, prompt)
}

func (c *Client) askClaude(ctx context.Context, prompt string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 2048,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", c.anthropicKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("claude: %s", out.Error.Message)
	}
	if len(out.Content) == 0 {
		return "", fmt.Errorf("claude: empty response")
	}
	return out.Content[0].Text, nil
}

func (c *Client) askOllama(ctx context.Context, prompt string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":    c.ollamaModel,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
		"stream":   false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.ollamaURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("ollama: empty response")
	}
	return out.Choices[0].Message.Content, nil
}

func (c *Client) askOpenAI(ctx context.Context, prompt string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.openaiKey)
	req.Header.Set("content-type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openai: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("openai: empty response")
	}
	return out.Choices[0].Message.Content, nil
}
