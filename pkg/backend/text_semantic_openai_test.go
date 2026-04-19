package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewOpenAITextSemanticGeneratorValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     OpenAITextSemanticGeneratorConfig
		wantErr bool
	}{
		{
			name:    "missing base url",
			cfg:     OpenAITextSemanticGeneratorConfig{APIKey: "secret", Model: "gpt-4.1-mini"},
			wantErr: true,
		},
		{
			name:    "missing api key",
			cfg:     OpenAITextSemanticGeneratorConfig{BaseURL: "https://example.com", Model: "gpt-4.1-mini"},
			wantErr: true,
		},
		{
			name:    "missing model",
			cfg:     OpenAITextSemanticGeneratorConfig{BaseURL: "https://example.com", APIKey: "secret"},
			wantErr: true,
		},
		{
			name:    "complete config",
			cfg:     OpenAITextSemanticGeneratorConfig{BaseURL: "https://example.com", APIKey: "secret", Model: "gpt-4.1-mini"},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewOpenAITextSemanticGenerator(tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("NewOpenAITextSemanticGenerator() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestOpenAITextSemanticGeneratorGenerateFileSemanticText(t *testing.T) {
	t.Parallel()

	var (
		gotModel     string
		gotAuth      string
		gotMaxTokens float64
		gotSystem    string
		gotUser      string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		gotAuth = r.Header.Get("Authorization")

		var payload struct {
			Model     string  `json:"model"`
			MaxTokens float64 `json:"max_tokens"`
			Messages  []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotModel = payload.Model
		gotMaxTokens = payload.MaxTokens
		if len(payload.Messages) != 2 {
			t.Fatalf("messages len = %d, want 2", len(payload.Messages))
		}
		gotSystem = payload.Messages[0].Content
		gotUser = payload.Messages[1].Content

		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"content":"semantic_text_format: drive9-file-semantic/v1\npurpose:\n- demo\nkey_topics:\n- alpha\nimportant_identifiers:\n- HandleRequest\nstructure:\n- section_1: intro\nsemantic_summary:\nsummary"}}],
			"usage":{"prompt_tokens":123,"completion_tokens":45}
		}`))
	}))
	defer srv.Close()

	generator, err := NewOpenAITextSemanticGenerator(OpenAITextSemanticGeneratorConfig{
		BaseURL:   srv.URL,
		APIKey:    "secret",
		Model:     "gpt-4.1-mini",
		Prompt:    "custom prompt",
		MaxTokens: 321,
		Client:    srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	text, usage, err := generator.GenerateFileSemanticText(context.Background(), TextSemanticRequest{
		Path:        "/docs/example.txt",
		ContentType: "text/plain",
		Data:        []byte("alpha\nbeta\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("authorization=%q, want bearer token", gotAuth)
	}
	if gotModel != "gpt-4.1-mini" {
		t.Fatalf("model=%q, want gpt-4.1-mini", gotModel)
	}
	if gotMaxTokens != 321 {
		t.Fatalf("max_tokens=%v, want 321", gotMaxTokens)
	}
	if gotSystem != "custom prompt" {
		t.Fatalf("system prompt=%q, want custom prompt", gotSystem)
	}
	for _, want := range []string{"Path: /docs/example.txt", "Content-Type: text/plain", "alpha\nbeta"} {
		if !strings.Contains(gotUser, want) {
			t.Fatalf("user prompt missing %q: %q", want, gotUser)
		}
	}
	if !strings.Contains(text, "drive9-file-semantic/v1") {
		t.Fatalf("text=%q, want semantic output", text)
	}
	if usage.PromptTokens != 123 || usage.CompletionTokens != 45 {
		t.Fatalf("usage=%+v, want prompt=123 completion=45", usage)
	}
}
