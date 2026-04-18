package backend

import (
	"context"
	"strings"
	"testing"

	"github.com/c4pt0r/agfs/agfs-server/pkg/filesystem"
)

type staticBackendTextSemanticGenerator struct {
	text string
	err  error
}

func (g staticBackendTextSemanticGenerator) GenerateFileSemanticText(_ context.Context, _ TextSemanticRequest) (string, TextSemanticUsage, error) {
	if g.err != nil {
		return "", TextSemanticUsage{}, g.err
	}
	return g.text, TextSemanticUsage{}, nil
}

func TestBasicTextSemanticGeneratorProducesStructuredText(t *testing.T) {
	t.Parallel()

	generator := NewBasicTextSemanticGenerator()
	text, _, err := generator.GenerateFileSemanticText(context.Background(), TextSemanticRequest{
		Path: "/docs/example.go",
		Data: []byte("package example\n\nfunc HandleRequest() error {\n\treturn nil\n}\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"semantic_text_format: drive9-file-semantic/v1",
		"purpose:",
		"key_topics:",
		"important_identifiers:",
		"structure:",
		"semantic_summary:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated text missing %q: %q", want, text)
		}
	}
}

func TestProcessFileSemanticTaskWritesContentText(t *testing.T) {
	b := newTestBackendWithOptions(t, Options{
		DatabaseAutoEmbedding: true,
		TextSemantic: TextSemanticOptions{
			Enabled:   true,
			Generator: staticBackendTextSemanticGenerator{text: "semantic_text_format: drive9-file-semantic/v1\npurpose:\n- test\nkey_topics:\n- topic\nimportant_identifiers:\n- ident\nstructure:\n- section\nsemantic_summary:\nsummary"},
		},
	})

	data := repeatedTextBytes(smallFileThreshold + 64)
	if _, err := b.Write("/docs/process.txt", data, 0, filesystem.WriteFlagCreate); err != nil {
		t.Fatal(err)
	}
	fileID, _, _, _ := mustFileForPath(t, b, "/docs/process.txt")
	result, err := b.ProcessFileSemanticTask(context.Background(), TextSemanticTaskSpec{
		FileID:      fileID,
		Path:        "/docs/process.txt",
		ContentType: "text/plain",
		Revision:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result != TextSemanticResultWritten {
		t.Fatalf("result=%q, want %q", result, TextSemanticResultWritten)
	}
	nf, err := b.Store().Stat(context.Background(), "/docs/process.txt")
	if err != nil || nf.File == nil {
		t.Fatalf("stat /docs/process.txt: %v", err)
	}
	if !strings.Contains(nf.File.ContentText, "drive9-file-semantic/v1") {
		t.Fatalf("content_text=%q, want generated semantic text", nf.File.ContentText)
	}
}
