package backend

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/mem9-ai/dat9/pkg/metrics"
	"github.com/mem9-ai/dat9/pkg/semantic"
)

func (b *Dat9Backend) enqueueEmbedTaskTx(tx *sql.Tx, fileID string, revision int64) error {
	now := time.Now().UTC()
	_, err := b.store.EnqueueSemanticTaskTx(tx, newEmbedTask(b.genID(), fileID, revision, now))
	return err
}

func (b *Dat9Backend) enqueueImgExtractTaskTx(tx *sql.Tx, fileID string, revision int64, path, contentType string) error {
	now := time.Now().UTC()
	task, err := newImgExtractTask(b.genID(), fileID, revision, path, contentType, now)
	if err != nil {
		return err
	}
	_, err = b.store.EnqueueSemanticTaskTx(tx, task)
	return err
}

func (b *Dat9Backend) enqueueAudioExtractTaskTx(tx *sql.Tx, fileID string, revision int64, path, contentType string) error {
	now := time.Now().UTC()
	task, err := newAudioExtractTask(b.genID(), fileID, revision, path, contentType, now)
	if err != nil {
		return err
	}
	_, err = b.store.EnqueueSemanticTaskTx(tx, task)
	return err
}

func (b *Dat9Backend) enqueueFileSemanticTaskTx(tx *sql.Tx, fileID string, revision int64, path, contentType string) error {
	now := time.Now().UTC()
	task, err := newFileSemanticTask(b.genID(), fileID, revision, path, contentType, now)
	if err != nil {
		return err
	}
	_, err = b.store.EnqueueSemanticTaskTx(tx, task)
	return err
}

// enqueueTiDBAutoSemanticTasksTx registers durable img_extract_text and/or
// audio_extract_text tasks and/or generate_file_semantic_text tasks for one
// confirmed file revision in TiDB auto-embedding mode. When the tenant's media
// LLM file quota is exceeded, media extraction tasks are skipped but file-level
// direct-text semantic tasks can still be enqueued because they use a separate
// runtime and ownership boundary.
func (b *Dat9Backend) enqueueTiDBAutoSemanticTasksTx(ctx context.Context, tx *sql.Tx, fileID string, revision int64, path, contentType string, size int64, contentText string) error {
	isImage := b.hasAsyncImageTextSource(path, contentType)
	isAudio := b.shouldEnqueueAudioExtractTask(path, contentType)
	isFileSemantic := b.SupportsFileSemanticTextGenerate() && shouldEnqueueFileSemanticTask(path, contentType, size, contentText)
	if !isImage && !isAudio && !isFileSemantic {
		return nil
	}
	if b.mediaLLMQuotaExceededCheckTx(ctx, tx) {
		metrics.RecordOperation("media_llm_budget", "enqueue_skip", "quota_exceeded", 0)
		isImage = false
		isAudio = false
	}
	if isImage {
		if err := b.enqueueImgExtractTaskTx(tx, fileID, revision, path, contentType); err != nil {
			return err
		}
	}
	if isAudio {
		if err := b.enqueueAudioExtractTaskTx(tx, fileID, revision, path, contentType); err != nil {
			return err
		}
	}
	if isFileSemantic {
		if err := b.enqueueFileSemanticTaskTx(tx, fileID, revision, path, contentType); err != nil {
			return err
		}
	}
	return nil
}

func (b *Dat9Backend) shouldEnqueueAudioExtractTask(path, contentType string) bool {
	if !b.SupportsAsyncAudioExtract() {
		return false
	}
	return isSupportedAudioForSemanticTask(path, contentType)
}

func newEmbedTask(taskID, fileID string, revision int64, now time.Time) *semantic.Task {
	now = now.UTC()
	return &semantic.Task{
		TaskID:          taskID,
		TaskType:        semantic.TaskTypeEmbed,
		ResourceID:      fileID,
		ResourceVersion: revision,
		Status:          semantic.TaskQueued,
		MaxAttempts:     5,
		AvailableAt:     now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}

func newImgExtractTask(taskID, fileID string, revision int64, path, contentType string, now time.Time) (*semantic.Task, error) {
	now = now.UTC()
	payload := semantic.ImgExtractTaskPayload{Path: path, ContentType: contentType}
	var payloadJSON []byte
	if payload.Path != "" || payload.ContentType != "" {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		payloadJSON = encoded
	}
	return &semantic.Task{
		TaskID:          taskID,
		TaskType:        semantic.TaskTypeImgExtractText,
		ResourceID:      fileID,
		ResourceVersion: revision,
		Status:          semantic.TaskQueued,
		MaxAttempts:     5,
		AvailableAt:     now,
		PayloadJSON:     payloadJSON,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

func newAudioExtractTask(taskID, fileID string, revision int64, path, contentType string, now time.Time) (*semantic.Task, error) {
	now = now.UTC()
	payload := semantic.AudioExtractTaskPayload{Path: path, ContentType: contentType}
	var payloadJSON []byte
	if payload.Path != "" || payload.ContentType != "" {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		payloadJSON = encoded
	}
	return &semantic.Task{
		TaskID:          taskID,
		TaskType:        semantic.TaskTypeAudioExtractText,
		ResourceID:      fileID,
		ResourceVersion: revision,
		Status:          semantic.TaskQueued,
		MaxAttempts:     5,
		AvailableAt:     now,
		PayloadJSON:     payloadJSON,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

func newFileSemanticTask(taskID, fileID string, revision int64, path, contentType string, now time.Time) (*semantic.Task, error) {
	now = now.UTC()
	payload := semantic.FileSemanticTaskPayload{Path: path, ContentType: contentType}
	var payloadJSON []byte
	if payload.Path != "" || payload.ContentType != "" {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		payloadJSON = encoded
	}
	return &semantic.Task{
		TaskID:          taskID,
		TaskType:        semantic.TaskTypeGenerateFileSemanticText,
		ResourceID:      fileID,
		ResourceVersion: revision,
		Status:          semantic.TaskQueued,
		MaxAttempts:     5,
		AvailableAt:     now,
		PayloadJSON:     payloadJSON,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

func (b *Dat9Backend) shouldEnqueueEmbedForRevision(path, contentType, contentText string) bool {
	if strings.TrimSpace(contentText) != "" {
		return true
	}
	return b.hasAsyncImageTextSource(path, contentType)
}

func (b *Dat9Backend) hasAsyncImageTextSource(path, contentType string) bool {
	if !b.imageExtractEnabled || b.imageExtractor == nil {
		return false
	}
	if isImageContentType(contentType) {
		return true
	}
	return isImageContentType(contentTypeFromPath(path))
}

// ConfiguredAutoSemanticTaskTypes returns the durable semantic task types
// implied by backend options before a Dat9Backend instance is constructed.
//
// This keeps startup validation, tenant-pool coarse routing, and per-backend
// capability exposure aligned on the same runtime viability checks.
func ConfiguredAutoSemanticTaskTypes(opts Options) []semantic.TaskType {
	var out []semantic.TaskType
	if AsyncImageExtractWillWireRuntime(opts.AsyncImageExtract) {
		out = append(out, semantic.TaskTypeImgExtractText)
	}
	if AsyncAudioExtractWillWireRuntime(opts.AsyncAudioExtract) {
		out = append(out, semantic.TaskTypeAudioExtractText)
	}
	if TextSemanticWillWireRuntime(opts.TextSemantic) {
		out = append(out, semantic.TaskTypeGenerateFileSemanticText)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// AutoSemanticTaskTypes returns durable semantic task types executed on the
// database auto-embedding (TiDB auto) path: img_extract_text and/or
// audio_extract_text and/or generate_file_semantic_text when the corresponding
// async runtimes are configured.
//
// It does not include app-managed embed work; embed routing uses the worker
// embedder, not the backend. A nil return means this backend contributes no
// auto semantic tasks. The returned slice must be treated as read-only.
func (b *Dat9Backend) AutoSemanticTaskTypes() []semantic.TaskType {
	if b == nil || !b.UsesDatabaseAutoEmbedding() {
		return nil
	}
	return ConfiguredAutoSemanticTaskTypes(Options{
		AsyncImageExtract: AsyncImageExtractOptions{Enabled: b.SupportsAsyncImageExtract()},
		AsyncAudioExtract: AsyncAudioExtractOptions{Enabled: b.SupportsAsyncAudioExtract(), Extractor: b.audioExtractor},
		TextSemantic:      TextSemanticOptions{Enabled: b.SupportsFileSemanticTextGenerate(), Generator: b.textSemanticGenerator},
	})
}
