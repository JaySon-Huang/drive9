package main

import (
	"os"
	"testing"

	"github.com/mem9-ai/dat9/pkg/backend"
)

func TestBuildBackendOptionsFromEnvTextSemanticDefaults(t *testing.T) {
	keys := []string{
		"DRIVE9_QUERY_EMBED_API_BASE",
		"DRIVE9_QUERY_EMBED_API_KEY",
		"DRIVE9_QUERY_EMBED_MODEL",
		"DRIVE9_IMAGE_EXTRACT_ENABLED",
		envAudioExtractEnabled,
		envAudioExtractMode,
		envAudioExtractAPIBase,
		envAudioExtractAPIKey,
		envAudioExtractModel,
		"DRIVE9_TEXT_SEMANTIC_ENABLED",
		"DRIVE9_TEXT_SEMANTIC_MAX_SOURCE_BYTES",
		"DRIVE9_TEXT_SEMANTIC_TIMEOUT_SECONDS",
		"DRIVE9_TEXT_SEMANTIC_MAX_TEXT_BYTES",
	}
	prev := make(map[string]string, len(keys))
	for _, k := range keys {
		prev[k] = os.Getenv(k)
	}
	t.Cleanup(func() {
		for _, k := range keys {
			if prev[k] == "" {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, prev[k])
			}
		}
	})
	for _, k := range keys {
		_ = os.Unsetenv(k)
	}

	if err := os.Setenv("DRIVE9_TEXT_SEMANTIC_ENABLED", "true"); err != nil {
		t.Fatal(err)
	}

	opts, err := buildBackendOptionsFromEnv()
	if err != nil {
		t.Fatalf("buildBackendOptionsFromEnv: %v", err)
	}
	if opts.TextSemantic.MaxSourceBytes != backend.DefaultTextSemanticMaxSourceBytes {
		t.Fatalf("MaxSourceBytes=%d, want %d", opts.TextSemantic.MaxSourceBytes, backend.DefaultTextSemanticMaxSourceBytes)
	}
	if got := opts.TextSemantic.TaskTimeout; got != backend.DefaultTextSemanticTimeout {
		t.Fatalf("TaskTimeout=%v, want %v", got, backend.DefaultTextSemanticTimeout)
	}
	if got := opts.TextSemantic.MaxGenerateTextBytes; got != backend.DefaultTextSemanticMaxGenerateTextBytes {
		t.Fatalf("MaxGenerateTextBytes=%d, want %d", got, backend.DefaultTextSemanticMaxGenerateTextBytes)
	}
}
