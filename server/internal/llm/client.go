package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type Client struct {
	apiKey      string
	baseURL     string
	model       string
	temperature float64
	http        *http.Client
}

type Config struct {
	APIKey      string
	BaseURL     string
	Model       string
	Temperature float64
}

func New(cfg Config) *Client {
	if cfg.APIKey == "" {
		return nil
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.openai.com/v1"
	}
	if cfg.Model == "" {
		cfg.Model = "gpt-4o-mini"
	}
	if cfg.Temperature <= 0 {
		cfg.Temperature = 0.1
	}
	return &Client{
		apiKey:      cfg.APIKey,
		baseURL:     strings.TrimRight(cfg.BaseURL, "/"),
		model:       cfg.Model,
		temperature: cfg.Temperature,
		http: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	messages := []Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}

	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Messages:    messages,
		Temperature: c.temperature,
	})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	var chatResp chatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if chatResp.Error != nil {
		return "", fmt.Errorf("llm error: %s", chatResp.Error.Message)
	}

	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("llm returned no choices")
	}

	return chatResp.Choices[0].Message.Content, nil
}

func StripMarkdownFences(s string) string {
	re := regexp.MustCompile("(?s)^\\s*```(?:json)?\\s*\n?(.*?)\\s*```\\s*$")
	if match := re.FindStringSubmatch(s); len(match) > 1 {
		return strings.TrimSpace(match[1])
	}
	return strings.TrimSpace(s)
}

func ParseJSON[T any](raw string) (T, error) {
	var result T
	cleaned := StripMarkdownFences(raw)
	if err := json.Unmarshal([]byte(cleaned), &result); err != nil {
		return result, fmt.Errorf("invalid JSON: %w", err)
	}
	return result, nil
}
