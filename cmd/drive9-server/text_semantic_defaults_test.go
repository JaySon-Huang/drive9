package main

import (
	"testing"

	"github.com/mem9-ai/dat9/pkg/backend"
)

func TestBuildBackendOptionsFromEnvTextSemanticDefaults(t *testing.T) {
	keys := []string{
		"DRIVE9_QUERY_EMBED_API_BASE",
		"DRIVE9_QUERY_EMBED_API_KEY",
		"DRIVE9_QUERY_EMBED_MODEL",
		"DRIVE9_IMAGE_EXTRACT_ENABLED",
		"DRIVE9_AUDIO_EXTRACT_ENABLED",
		"DRIVE9_TEXT_SEMANTIC_ENABLED",
		"DRIVE9_TEXT_SEMANTIC_MAX_SOURCE_BYTES",
		"DRIVE9_TEXT_SEMANTIC_TIMEOUT_SECONDS",
		"DRIVE9_TEXT_SEMANTIC_MAX_TEXT_BYTES",
	}
	restore := snapshotEnv(t, keys)
	t.Cleanup(func() { restoreEnv(t, restore) })
	unsetEnv(t, keys)

	setEnv(t, "DRIVE9_TEXT_SEMANTIC_ENABLED", "true")

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
