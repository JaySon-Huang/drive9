package backend

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/c4pt0r/agfs/agfs-server/pkg/filesystem"
	"github.com/mem9-ai/dat9/internal/testmysql"
	"github.com/mem9-ai/dat9/pkg/datastore"
	"github.com/mem9-ai/dat9/pkg/s3client"
)

type staticBackendTextSemanticGenerator struct {
	text  string
	usage TextSemanticUsage
	err   error
	calls int
}

func (g *staticBackendTextSemanticGenerator) GenerateFileSemanticText(_ context.Context, _ TextSemanticRequest) (string, TextSemanticUsage, error) {
	g.calls++
	if g.err != nil {
		return "", TextSemanticUsage{}, g.err
	}
	return g.text, g.usage, nil
}

type captureBackendTextSemanticGenerator struct {
	lastRequest TextSemanticRequest
	text        string
}

func (g *captureBackendTextSemanticGenerator) GenerateFileSemanticText(_ context.Context, req TextSemanticRequest) (string, TextSemanticUsage, error) {
	g.lastRequest = req
	return g.text, TextSemanticUsage{}, nil
}

type failAfterReadS3Client struct {
	data      map[string][]byte
	failAfter int64
}

func newFailAfterReadS3Client(failAfter int64) *failAfterReadS3Client {
	return &failAfterReadS3Client{data: make(map[string][]byte), failAfter: failAfter}
}

func (c *failAfterReadS3Client) CreateMultipartUpload(context.Context, string, s3client.ChecksumAlgo) (*s3client.MultipartUpload, error) {
	return nil, errors.New("not implemented")
}

func (c *failAfterReadS3Client) PresignUploadPart(context.Context, string, string, int, int64, s3client.ChecksumAlgo, string, time.Duration) (*s3client.UploadPartURL, error) {
	return nil, errors.New("not implemented")
}

func (c *failAfterReadS3Client) CompleteMultipartUpload(context.Context, string, string, []s3client.Part) error {
	return errors.New("not implemented")
}

func (c *failAfterReadS3Client) AbortMultipartUpload(context.Context, string, string) error {
	return errors.New("not implemented")
}

func (c *failAfterReadS3Client) ListParts(context.Context, string, string) ([]s3client.Part, error) {
	return nil, errors.New("not implemented")
}

func (c *failAfterReadS3Client) PresignGetObject(context.Context, string, time.Duration) (string, error) {
	return "", errors.New("not implemented")
}

func (c *failAfterReadS3Client) PutObject(_ context.Context, key string, body io.Reader, _ int64) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	c.data[key] = append([]byte(nil), data...)
	return nil
}

func (c *failAfterReadS3Client) GetObject(_ context.Context, key string) (io.ReadCloser, error) {
	data, ok := c.data[key]
	if !ok {
		return nil, errors.New("not found")
	}
	return &failAfterReadCloser{data: append([]byte(nil), data...), failAfter: c.failAfter}, nil
}

func (c *failAfterReadS3Client) DeleteObject(context.Context, string) error {
	return errors.New("not implemented")
}

func (c *failAfterReadS3Client) UploadPartCopy(context.Context, string, string, int, string, int64, int64) (string, error) {
	return "", errors.New("not implemented")
}

func (c *failAfterReadS3Client) PresignGetObjectRange(context.Context, string, int64, int64, time.Duration) (string, error) {
	return "", errors.New("not implemented")
}

type failAfterReadCloser struct {
	data      []byte
	offset    int64
	failAfter int64
}

func (r *failAfterReadCloser) Read(p []byte) (int, error) {
	if r.offset >= int64(len(r.data)) {
		return 0, io.EOF
	}
	if r.failAfter >= 0 && r.offset >= r.failAfter {
		return 0, errors.New("read beyond configured limit")
	}
	remaining := int64(len(r.data)) - r.offset
	n := len(p)
	if int64(n) > remaining {
		n = int(remaining)
	}
	if r.failAfter >= 0 {
		allowed := r.failAfter - r.offset
		if allowed < int64(n) {
			n = int(allowed)
		}
	}
	copy(p, r.data[r.offset:r.offset+int64(n)])
	r.offset += int64(n)
	return n, nil
}

func (r *failAfterReadCloser) Close() error { return nil }

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
	generator := &staticBackendTextSemanticGenerator{
		text:  "semantic_text_format: drive9-file-semantic/v1\npurpose:\n- test\nkey_topics:\n- topic\nimportant_identifiers:\n- ident\nstructure:\n- section\nsemantic_summary:\nsummary",
		usage: TextSemanticUsage{PromptTokens: 80, CompletionTokens: 40},
	}
	b := newTestBackendWithOptions(t, Options{
		DatabaseAutoEmbedding: true,
		LLMCostBudget: LLMCostBudgetOptions{
			TextSemanticCostPerKTokenMillicents: 1000,
		},
		TextSemantic: TextSemanticOptions{
			Enabled:   true,
			Generator: generator,
		},
	})

	data := repeatedTextBytes(smallFileThreshold + 64)
	if _, err := b.Write("/docs/process.txt", data, 0, filesystem.WriteFlagCreate); err != nil {
		t.Fatal(err)
	}
	fileID, _, _, _ := mustFileForPath(t, b, "/docs/process.txt")
	result, err := b.ProcessFileSemanticTask(context.Background(), TextSemanticTaskSpec{
		TaskID:      "text-task-1",
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
	usage := mustLLMUsageRowForTask(t, b.Store().DB(), "text-task-1")
	if usage.TaskType != "generate_file_semantic_text" {
		t.Fatalf("task_type=%q, want generate_file_semantic_text", usage.TaskType)
	}
	if usage.RawUnits != 120 || usage.RawUnitType != "tokens" {
		t.Fatalf("raw_units=%d raw_unit_type=%q, want 120/tokens", usage.RawUnits, usage.RawUnitType)
	}
	if usage.CostMillicents != 120 {
		t.Fatalf("cost=%d, want 120", usage.CostMillicents)
	}
}

func TestProcessFileSemanticTaskUsesBoundedS3Read(t *testing.T) {
	s3c := newFailAfterReadS3Client(64)
	store := newTestStoreForTextSemantic(t)
	generator := &captureBackendTextSemanticGenerator{
		text: "semantic_text_format: drive9-file-semantic/v1\npurpose:\n- bounded\nkey_topics:\n- chunk\nimportant_identifiers:\n- value\nstructure:\n- section\nsemantic_summary:\nsummary",
	}
	b, err := NewWithS3ModeAndOptions(store, s3c, true, Options{
		DatabaseAutoEmbedding: true,
		TextSemantic: TextSemanticOptions{
			Enabled:              true,
			MaxSourceBytes:       64,
			MaxGenerateTextBytes: 16 << 10,
			Generator:            generator,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })

	data := []byte(strings.Repeat("0123456789", 20))
	if _, err := b.Write("/docs/bounded.txt", data, 0, filesystem.WriteFlagCreate); err != nil {
		t.Fatal(err)
	}
	fileID, _, _, _ := mustFileForPath(t, b, "/docs/bounded.txt")
	result, err := b.ProcessFileSemanticTask(context.Background(), TextSemanticTaskSpec{
		FileID:      fileID,
		Path:        "/docs/bounded.txt",
		ContentType: "text/plain",
		Revision:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result != TextSemanticResultWritten {
		t.Fatalf("result=%q, want %q", result, TextSemanticResultWritten)
	}
	if got := len(generator.lastRequest.Data); got != 64 {
		t.Fatalf("generator source bytes=%d, want 64", got)
	}
	if string(generator.lastRequest.Data) != string(data[:64]) {
		t.Fatalf("generator source mismatch, got %q want prefix %q", string(generator.lastRequest.Data), string(data[:64]))
	}
}

func TestProcessFileSemanticTaskBudgetExhausted(t *testing.T) {
	generator := &staticBackendTextSemanticGenerator{
		text: "semantic_text_format: drive9-file-semantic/v1\nsemantic_summary:\nsummary",
	}
	b := newTestBackendWithOptions(t, Options{
		DatabaseAutoEmbedding: true,
		LLMCostBudget: LLMCostBudgetOptions{
			MaxMonthlyMillicents:                1,
			TextSemanticCostPerKTokenMillicents: 1000,
		},
		TextSemantic: TextSemanticOptions{
			Enabled:   true,
			Generator: generator,
		},
	})
	if err := b.Store().InsertLLMUsage("img_extract_text", "spent", 2, 1, "tokens"); err != nil {
		t.Fatal(err)
	}

	data := repeatedTextBytes(smallFileThreshold + 64)
	if _, err := b.Write("/docs/budget.txt", data, 0, filesystem.WriteFlagCreate); err != nil {
		t.Fatal(err)
	}
	fileID, _, _, _ := mustFileForPath(t, b, "/docs/budget.txt")
	result, err := b.ProcessFileSemanticTask(context.Background(), TextSemanticTaskSpec{
		TaskID:      "text-task-budget",
		FileID:      fileID,
		Path:        "/docs/budget.txt",
		ContentType: "text/plain",
		Revision:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result != TextSemanticResultBudgetExhausted {
		t.Fatalf("result=%q, want %q", result, TextSemanticResultBudgetExhausted)
	}
	if generator.calls != 0 {
		t.Fatalf("generator calls=%d, want 0", generator.calls)
	}
	if count := countLLMUsageRowsForTask(t, b.Store().DB(), "text-task-budget"); count != 0 {
		t.Fatalf("llm_usage rows=%d, want 0", count)
	}
}

func TestProcessFileSemanticTaskFallsBackToFixedCost(t *testing.T) {
	generator := &staticBackendTextSemanticGenerator{
		text: "semantic_text_format: drive9-file-semantic/v1\nsemantic_summary:\nsummary",
	}
	b := newTestBackendWithOptions(t, Options{
		DatabaseAutoEmbedding: true,
		LLMCostBudget: LLMCostBudgetOptions{
			FallbackTextSemanticCostMillicents: 77,
		},
		TextSemantic: TextSemanticOptions{
			Enabled:   true,
			Generator: generator,
		},
	})

	data := repeatedTextBytes(smallFileThreshold + 64)
	if _, err := b.Write("/docs/fallback.txt", data, 0, filesystem.WriteFlagCreate); err != nil {
		t.Fatal(err)
	}
	fileID, _, _, _ := mustFileForPath(t, b, "/docs/fallback.txt")
	result, err := b.ProcessFileSemanticTask(context.Background(), TextSemanticTaskSpec{
		TaskID:      "text-task-fallback",
		FileID:      fileID,
		Path:        "/docs/fallback.txt",
		ContentType: "text/plain",
		Revision:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result != TextSemanticResultWritten {
		t.Fatalf("result=%q, want %q", result, TextSemanticResultWritten)
	}
	usage := mustLLMUsageRowForTask(t, b.Store().DB(), "text-task-fallback")
	if usage.RawUnits != 0 || usage.RawUnitType != "fallback" {
		t.Fatalf("raw_units=%d raw_unit_type=%q, want 0/fallback", usage.RawUnits, usage.RawUnitType)
	}
	if usage.CostMillicents != 77 {
		t.Fatalf("cost=%d, want 77", usage.CostMillicents)
	}
}

type llmUsageRow struct {
	TaskType       string
	TaskID         string
	CostMillicents int64
	RawUnits       int64
	RawUnitType    string
}

func mustLLMUsageRowForTask(t *testing.T, db *sql.DB, taskID string) llmUsageRow {
	t.Helper()
	var row llmUsageRow
	err := db.QueryRow(`SELECT task_type, task_id, cost_millicents, raw_units, raw_unit_type
		FROM llm_usage WHERE task_id = ? ORDER BY created_at DESC LIMIT 1`, taskID).Scan(
		&row.TaskType, &row.TaskID, &row.CostMillicents, &row.RawUnits, &row.RawUnitType,
	)
	if err != nil {
		t.Fatalf("load llm_usage for %s: %v", taskID, err)
	}
	return row
}

func countLLMUsageRowsForTask(t *testing.T, db *sql.DB, taskID string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM llm_usage WHERE task_id = ?`, taskID).Scan(&count); err != nil {
		t.Fatalf("count llm_usage for %s: %v", taskID, err)
	}
	return count
}

func newTestStoreForTextSemantic(t *testing.T) *datastore.Store {
	t.Helper()
	initBackendSchema(t, testDSN)
	store, err := datastore.Open(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	testmysql.ResetDB(t, store.DB())
	t.Cleanup(func() { _ = store.Close() })
	return store
}
