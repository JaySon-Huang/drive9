package backend

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mem9-ai/dat9/pkg/datastore"
	"github.com/mem9-ai/dat9/pkg/metrics"
)

// TextSemanticRequest is the input to a pluggable file-level semantic text generator.
type TextSemanticRequest struct {
	FileID      string
	Path        string
	ContentType string
	Data        []byte
}

// TextSemanticUsage reports the resource consumption of one file semantic text generation call.
type TextSemanticUsage struct {
	PromptTokens     int
	CompletionTokens int
}

// TotalTokens returns the sum of prompt and completion tokens.
func (u TextSemanticUsage) TotalTokens() int { return u.PromptTokens + u.CompletionTokens }

// TextSemanticGenerator generates bounded retrieval-oriented semantic text for one file revision.
type TextSemanticGenerator interface {
	GenerateFileSemanticText(ctx context.Context, req TextSemanticRequest) (string, TextSemanticUsage, error)
}

// TextSemanticTaskSpec carries the revision-scoped inputs needed to generate
// file-level semantic text for one file version.
type TextSemanticTaskSpec struct {
	// TaskID is the durable semantic_tasks identity for this generation job.
	TaskID      string
	FileID      string
	Path        string
	ContentType string
	Revision    int64
}

// TextSemanticResult reports the outcome of one file semantic text generation attempt.
type TextSemanticResult string

const (
	TextSemanticResultRuntimeNotConfigured TextSemanticResult = "runtime_not_configured"
	TextSemanticResultGetFileError         TextSemanticResult = "get_file_error"
	TextSemanticResultFileNotFound         TextSemanticResult = "file_not_found"
	TextSemanticResultNotConfirmed         TextSemanticResult = "not_confirmed"
	TextSemanticResultNotDirectText        TextSemanticResult = "not_direct_text"
	TextSemanticResultStale                TextSemanticResult = "stale"
	TextSemanticResultLoadError            TextSemanticResult = "load_error"
	TextSemanticResultGenerateError        TextSemanticResult = "generate_error"
	TextSemanticResultEmptyText            TextSemanticResult = "empty_text"
	TextSemanticResultUpdateError          TextSemanticResult = "update_error"
	TextSemanticResultWritten              TextSemanticResult = "written"
	TextSemanticResultBudgetExhausted      TextSemanticResult = "budget_exhausted"
)

var identifierPattern = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_./:-]*`)

// BasicTextSemanticGenerator provides a deterministic, no-network fallback
// generator for file-level semantic text. It trades quality for predictable
// bounded output so the semantic closure can function without an external model.
type BasicTextSemanticGenerator struct{}

// NewBasicTextSemanticGenerator returns the built-in deterministic text semantic generator.
func NewBasicTextSemanticGenerator() *BasicTextSemanticGenerator {
	return &BasicTextSemanticGenerator{}
}

// GenerateFileSemanticText implements TextSemanticGenerator.
func (g *BasicTextSemanticGenerator) GenerateFileSemanticText(_ context.Context, req TextSemanticRequest) (string, TextSemanticUsage, error) {
	normalized := normalizeTextSemanticSource(string(req.Data))
	if normalized == "" {
		return "", TextSemanticUsage{}, nil
	}

	lines := collectNonEmptyLines(normalized, 8)
	identifiers := collectIdentifiers(normalized, 12)
	base := path.Base(req.Path)
	if base == "." || base == "/" {
		base = req.Path
	}

	var b strings.Builder
	b.WriteString("semantic_text_format: drive9-file-semantic/v1\n")
	b.WriteString("purpose:\n")
	if len(lines) > 0 {
		fmt.Fprintf(&b, "- %s stores direct-text content related to %s.\n", base, shortenSemanticLine(lines[0], 180))
	} else {
		fmt.Fprintf(&b, "- %s stores direct-text content.\n", base)
	}
	b.WriteString("key_topics:\n")
	for _, line := range lines {
		fmt.Fprintf(&b, "- %s\n", shortenSemanticLine(line, 160))
	}
	b.WriteString("important_identifiers:\n")
	if len(identifiers) == 0 {
		b.WriteString("- none\n")
	} else {
		for _, ident := range identifiers {
			fmt.Fprintf(&b, "- %s\n", ident)
		}
	}
	b.WriteString("structure:\n")
	for idx, line := range lines {
		fmt.Fprintf(&b, "- section_%d: %s\n", idx+1, shortenSemanticLine(line, 140))
	}
	b.WriteString("semantic_summary:\n")
	if len(lines) > 0 {
		b.WriteString(strings.Join(lines, "\n"))
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String()), TextSemanticUsage{}, nil
}

// SupportsFileSemanticTextGenerate reports whether this backend instance has a
// fully wired file-level semantic text generation runtime.
func (b *Dat9Backend) SupportsFileSemanticTextGenerate() bool {
	return b != nil && b.textSemanticEnabled && b.textSemanticGenerator != nil
}

// ProcessFileSemanticTask runs the backend-owned file semantic text generation
// logic for one revision-scoped task.
func (b *Dat9Backend) ProcessFileSemanticTask(ctx context.Context, task TextSemanticTaskSpec) (TextSemanticResult, error) {
	if !b.SupportsFileSemanticTextGenerate() {
		return TextSemanticResultRuntimeNotConfigured, fmt.Errorf("text semantic runtime not configured")
	}
	if b.monthlyLLMCostExceeded() {
		metrics.RecordOperation("llm_cost_budget", "process_skip", "budget_exhausted", 0)
		return TextSemanticResultBudgetExhausted, nil
	}

	f, err := b.store.GetFile(ctx, task.FileID)
	if err != nil {
		if errors.Is(err, datastore.ErrNotFound) {
			return TextSemanticResultFileNotFound, nil
		}
		return TextSemanticResultGetFileError, fmt.Errorf("get file: %w", err)
	}
	if f.Status != datastore.StatusConfirmed {
		return TextSemanticResultNotConfirmed, nil
	}
	contentType := f.ContentType
	if contentType == "" {
		contentType = task.ContentType
	}
	if !isDirectTextSemanticCandidate(task.Path, contentType) {
		return TextSemanticResultNotDirectText, nil
	}
	if task.Revision > 0 && f.Revision != task.Revision {
		return TextSemanticResultStale, nil
	}

	data, err := b.loadTextSemanticSourceBytes(ctx, f)
	if err != nil {
		return TextSemanticResultLoadError, fmt.Errorf("load text semantic source: %w", err)
	}
	if len(data) == 0 {
		return TextSemanticResultEmptyText, nil
	}

	taskCtx, cancel := context.WithTimeout(ctx, b.textSemanticTimeout)
	text, usage, err := b.textSemanticGenerator.GenerateFileSemanticText(taskCtx, TextSemanticRequest{
		FileID:      task.FileID,
		Path:        task.Path,
		ContentType: contentType,
		Data:        data,
	})
	cancel()
	if err != nil {
		return TextSemanticResultGenerateError, fmt.Errorf("generate file semantic text: %w", err)
	}
	b.recordTextSemanticUsage(task.TaskID, usage)
	text = sanitizeGeneratedFileSemanticText(text, b.maxTextSemanticTextBytes)
	if text == "" {
		return TextSemanticResultEmptyText, nil
	}

	var updated bool
	err = b.store.InTx(ctx, func(tx *sql.Tx) error {
		var txErr error
		updated, txErr = b.store.UpdateFileSearchTextTx(tx, task.FileID, task.Revision, text)
		return txErr
	})
	if err != nil {
		return TextSemanticResultUpdateError, fmt.Errorf("update file search text: %w", err)
	}
	if !updated {
		return TextSemanticResultStale, nil
	}
	return TextSemanticResultWritten, nil
}

func (b *Dat9Backend) loadTextSemanticSourceBytes(ctx context.Context, f *datastore.File) ([]byte, error) {
	return b.readFileDataUpToCtx(ctx, f, b.textSemanticMaxSourceBytes)
}

func sanitizeGeneratedFileSemanticText(text string, maxBytes int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if maxBytes <= 0 || len([]byte(text)) <= maxBytes {
		return text
	}
	trimmed := []byte(text)
	trimmed = trimmed[:maxBytes]
	for len(trimmed) > 0 && !utf8.Valid(trimmed) {
		trimmed = trimmed[:len(trimmed)-1]
	}
	return strings.TrimSpace(string(trimmed))
}

func normalizeTextSemanticSource(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.TrimSpace(text)
}

func collectNonEmptyLines(text string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, limit)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, shortenSemanticLine(line, 200))
		if len(out) == limit {
			break
		}
	}
	return out
}

func collectIdentifiers(text string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	matches := identifierPattern.FindAllString(text, -1)
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, limit)
	for _, match := range matches {
		match = strings.TrimSpace(match)
		if len(match) < 3 {
			continue
		}
		if _, ok := seen[match]; ok {
			continue
		}
		seen[match] = struct{}{}
		out = append(out, match)
		if len(out) == limit {
			break
		}
	}
	return out
}

func shortenSemanticLine(line string, maxRunes int) string {
	line = strings.TrimSpace(line)
	if maxRunes <= 0 {
		return line
	}
	runes := []rune(line)
	if len(runes) <= maxRunes {
		return line
	}
	return strings.TrimSpace(string(runes[:maxRunes])) + "..."
}
