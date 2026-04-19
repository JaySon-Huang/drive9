package backend

import (
	"context"
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
	text string
	err  error
}

func (g staticBackendTextSemanticGenerator) GenerateFileSemanticText(_ context.Context, _ TextSemanticRequest) (string, TextSemanticUsage, error) {
	if g.err != nil {
		return "", TextSemanticUsage{}, g.err
	}
	return g.text, TextSemanticUsage{}, nil
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
