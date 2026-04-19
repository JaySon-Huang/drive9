package backend

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

const (
	defaultTextSemanticPrompt    = "You generate retrieval-oriented semantic text for one direct-text file. Return plain text only. Use exactly this structure:\nsemantic_text_format: drive9-file-semantic/v1\npurpose:\n- ...\nkey_topics:\n- ...\nimportant_identifiers:\n- ...\nstructure:\n- ...\nsemantic_summary:\n...\nKeep the output concise, factual, and useful for search. Do not wrap the answer in markdown fences."
	defaultTextSemanticMaxTokens = 512
)

// OpenAITextSemanticGeneratorConfig configures an OpenAI-compatible text
// generation endpoint for file-level semantic text generation.
type OpenAITextSemanticGeneratorConfig struct {
	BaseURL   string
	APIKey    string
	Model     string
	Prompt    string
	MaxTokens int
	Timeout   time.Duration
	Client    *http.Client
}

// OpenAITextSemanticGenerator generates retrieval-oriented semantic text for
// direct-text files via an OpenAI-compatible /v1/chat/completions API.
type OpenAITextSemanticGenerator struct {
	endpoint  string
	apiKey    string
	model     string
	prompt    string
	maxTokens int
	client    *http.Client
}

// NewOpenAITextSemanticGenerator builds a file semantic text generator backed
// by an OpenAI-compatible chat completion endpoint.
func NewOpenAITextSemanticGenerator(cfg OpenAITextSemanticGeneratorConfig) (*OpenAITextSemanticGenerator, error) {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		return nil, fmt.Errorf("text semantic generator base url is required")
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("text semantic generator api key is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("text semantic generator model is required")
	}

	var endpoint string
	if strings.HasSuffix(base, "/v1") {
		endpoint = base + "/chat/completions"
	} else {
		endpoint = base + "/v1/chat/completions"
	}
	if strings.TrimSpace(cfg.Prompt) == "" {
		cfg.Prompt = defaultTextSemanticPrompt
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = defaultTextSemanticMaxTokens
	}

	client := cfg.Client
	if client == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = defaultTextSemanticTimeout
		}
		client = &http.Client{Timeout: timeout}
	}

	return &OpenAITextSemanticGenerator{
		endpoint:  endpoint,
		apiKey:    cfg.APIKey,
		model:     cfg.Model,
		prompt:    cfg.Prompt,
		maxTokens: cfg.MaxTokens,
		client:    client,
	}, nil
}

// GenerateFileSemanticText implements TextSemanticGenerator.
func (g *OpenAITextSemanticGenerator) GenerateFileSemanticText(ctx context.Context, req TextSemanticRequest) (string, TextSemanticUsage, error) {
	payload := map[string]any{
		"model": g.model,
		"messages": []map[string]any{
			{
				"role":    "system",
				"content": g.prompt,
			},
			{
				"role":    "user",
				"content": buildTextSemanticUserPrompt(req),
			},
		},
		"temperature": 0,
		"max_tokens":  g.maxTokens,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", TextSemanticUsage{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", TextSemanticUsage{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+g.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return "", TextSemanticUsage{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", TextSemanticUsage{}, err
	}
	var parsed struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		if resp.StatusCode >= 300 {
			return "", TextSemanticUsage{}, fmt.Errorf("text semantic api status %d: %s", resp.StatusCode, truncateString(string(raw), 256))
		}
		return "", TextSemanticUsage{}, fmt.Errorf("decode text semantic response: %w", err)
	}
	if resp.StatusCode >= 300 {
		if parsed.Error != nil && parsed.Error.Message != "" {
			return "", TextSemanticUsage{}, fmt.Errorf("text semantic api status %d: %s", resp.StatusCode, parsed.Error.Message)
		}
		return "", TextSemanticUsage{}, fmt.Errorf("text semantic api status %d", resp.StatusCode)
	}
	if len(parsed.Choices) == 0 {
		return "", TextSemanticUsage{}, fmt.Errorf("text semantic api returned no choices")
	}
	text := extractOpenAIContentText(parsed.Choices[0].Message.Content)
	if strings.TrimSpace(text) == "" {
		return "", TextSemanticUsage{}, fmt.Errorf("text semantic api returned empty text")
	}

	var usage TextSemanticUsage
	if parsed.Usage != nil {
		usage.PromptTokens = parsed.Usage.PromptTokens
		usage.CompletionTokens = parsed.Usage.CompletionTokens
	}
	return text, usage, nil
}

func buildTextSemanticUserPrompt(req TextSemanticRequest) string {
	var b strings.Builder
	b.WriteString("Generate retrieval-oriented semantic text for this file.\n")
	fmt.Fprintf(&b, "Path: %s\n", req.Path)
	if strings.TrimSpace(req.ContentType) != "" {
		fmt.Fprintf(&b, "Content-Type: %s\n", req.ContentType)
	}
	b.WriteString("Source text follows verbatim.\n<source_text>\n")
	b.Write(req.Data)
	b.WriteString("\n</source_text>")
	return b.String()
}
