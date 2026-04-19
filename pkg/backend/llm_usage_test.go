package backend

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/mem9-ai/dat9/pkg/datastore"
)

// mockQuotaStore is a minimal test double for MetaQuotaStore that only
// implements MonthlyLLMCostMillicents. All other methods panic if called.
type mockQuotaStore struct {
	MetaQuotaStore // embed interface; unimplemented methods will nil-panic if called
	monthlyCost    int64
	monthlyCostErr error
}

func (m *mockQuotaStore) MonthlyLLMCostMillicents(_ context.Context, _ string) (int64, error) {
	return m.monthlyCost, m.monthlyCostErr
}

type mockLLMInsert struct {
	TenantID, TaskType, TaskID string
	CostMillicents, RawUnits   int64
	RawUnitType                string
}

type mockMetaLLMStore struct {
	mu       sync.Mutex
	inserts  []mockLLMInsert
	monthly  int64
	queryErr error
}

func (m *mockMetaLLMStore) InsertLLMUsage(_ context.Context, tenantID, taskType, taskID string, costMillicents, rawUnits int64, rawUnitType string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inserts = append(m.inserts, mockLLMInsert{
		TenantID: tenantID, TaskType: taskType, TaskID: taskID,
		CostMillicents: costMillicents, RawUnits: rawUnits, RawUnitType: rawUnitType,
	})
	return nil
}

func (m *mockMetaLLMStore) MonthlyLLMCostMillicents(_ context.Context, _ string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.monthly, m.queryErr
}

func (m *mockQuotaStore) GetQuotaConfig(_ context.Context, _ string) (*QuotaConfigView, error) {
	return &QuotaConfigView{}, nil
}

func (m *mockQuotaStore) GetQuotaUsage(_ context.Context, _ string) (*QuotaUsageView, error) {
	return &QuotaUsageView{}, nil
}

func (m *mockQuotaStore) EnsureQuotaUsageRow(_ context.Context, _ string) error         { return nil }
func (m *mockQuotaStore) IncrStorageBytes(_ context.Context, _ string, _ int64) error   { return nil }
func (m *mockQuotaStore) IncrReservedBytes(_ context.Context, _ string, _ int64) error  { return nil }
func (m *mockQuotaStore) IncrMediaFileCount(_ context.Context, _ string, _ int64) error { return nil }
func (m *mockQuotaStore) TransferReservedToConfirmed(_ context.Context, _ string, _, _ int64) error {
	return nil
}
func (m *mockQuotaStore) AtomicReserveAndInsertUpload(_ context.Context, _ *UploadReservationView) error {
	return nil
}
func (m *mockQuotaStore) IncrStorageBytesTx(_ *sql.Tx, _ string, _ int64) error   { return nil }
func (m *mockQuotaStore) IncrReservedBytesTx(_ *sql.Tx, _ string, _ int64) error  { return nil }
func (m *mockQuotaStore) IncrMediaFileCountTx(_ *sql.Tx, _ string, _ int64) error { return nil }
func (m *mockQuotaStore) TransferReservedToConfirmedTx(_ *sql.Tx, _ string, _, _ int64) error {
	return nil
}

func (m *mockQuotaStore) UpsertFileMeta(_ context.Context, _ *FileMetaView) error { return nil }
func (m *mockQuotaStore) GetFileMeta(_ context.Context, _, _ string) (*FileMetaView, error) {
	return nil, nil
}
func (m *mockQuotaStore) DeleteFileMeta(_ context.Context, _, _ string) error { return nil }
func (m *mockQuotaStore) UpsertFileMetaTx(_ *sql.Tx, _ *FileMetaView) error   { return nil }
func (m *mockQuotaStore) DeleteFileMetaTx(_ *sql.Tx, _, _ string) error       { return nil }

func (m *mockQuotaStore) InsertUploadReservation(_ context.Context, _ *UploadReservationView) error {
	return nil
}
func (m *mockQuotaStore) UpdateUploadReservationStatus(_ context.Context, _, _, _ string) error {
	return nil
}
func (m *mockQuotaStore) SettleActiveReservationTx(_ *sql.Tx, _, _, _ string) (bool, error) {
	return false, nil
}
func (m *mockQuotaStore) GetUploadReservation(_ context.Context, _, _ string) (*UploadReservationView, error) {
	return nil, nil
}

func (m *mockQuotaStore) InsertCentralLLMUsage(_ context.Context, _ *LLMUsageView) error { return nil }
func (m *mockQuotaStore) IncrMonthlyLLMCost(_ context.Context, _ string, _ int64) error  { return nil }
func (m *mockQuotaStore) InsertCentralLLMUsageTx(_ *sql.Tx, _ *LLMUsageView) error       { return nil }
func (m *mockQuotaStore) IncrMonthlyLLMCostTx(_ *sql.Tx, _ string, _ int64) error        { return nil }

func (m *mockQuotaStore) InsertMutationLog(_ context.Context, _ *MutationLogView) (int64, error) {
	return 0, nil
}
func (m *mockQuotaStore) InTx(_ context.Context, fn func(*sql.Tx) error) error {
	return fn(nil)
}
func (m *mockQuotaStore) SetQuotaCounters(_ context.Context, _ string, _, _ int64) error { return nil }

func TestMonthlyLLMCostExceeded_ServerQuota(t *testing.T) {
	mock := &mockQuotaStore{monthlyCost: 5000}
	b := &Dat9Backend{
		metaStore:                   mock,
		tenantID:                    "tenant-1",
		quotaSource:                 QuotaSourceServer,
		maxMonthlyLLMCostMillicents: 4000,
	}
	if !b.monthlyLLMCostExceeded() {
		t.Fatal("expected exceeded, got false")
	}
}

func TestInsertLLMUsage_TextSemanticMetaStore(t *testing.T) {
	mock := &mockMetaLLMStore{}
	b := &Dat9Backend{
		metaLLMStore:                        mock,
		tenantID:                            "tenant-1",
		textSemanticCostPerKTokenMillicents: 250,
	}
	b.recordTextSemanticUsage("task-3", TextSemanticUsage{PromptTokens: 600, CompletionTokens: 200})

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.inserts) != 1 {
		t.Fatalf("expected 1 insert, got %d", len(mock.inserts))
	}
	ins := mock.inserts[0]
	if ins.TaskType != "generate_file_semantic_text" {
		t.Fatalf("task_type=%q, want generate_file_semantic_text", ins.TaskType)
	}
	if ins.TaskID != "task-3" {
		t.Fatalf("task_id=%q, want task-3", ins.TaskID)
	}
	if ins.RawUnits != 800 || ins.RawUnitType != "tokens" {
		t.Fatalf("raw_units=%d raw_unit_type=%q, want 800/tokens", ins.RawUnits, ins.RawUnitType)
	}
	if ins.CostMillicents != 200 {
		t.Fatalf("cost=%d, want 200", ins.CostMillicents)
	}
}

func TestInsertLLMUsage_TextSemanticFallbackMetaStore(t *testing.T) {
	mock := &mockMetaLLMStore{}
	b := &Dat9Backend{
		metaLLMStore:                       mock,
		tenantID:                           "tenant-1",
		fallbackTextSemanticCostMillicents: 321,
	}
	b.recordTextSemanticUsage("task-4", TextSemanticUsage{})

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.inserts) != 1 {
		t.Fatalf("expected 1 insert, got %d", len(mock.inserts))
	}
	ins := mock.inserts[0]
	if ins.TaskType != "generate_file_semantic_text" {
		t.Fatalf("task_type=%q, want generate_file_semantic_text", ins.TaskType)
	}
	if ins.RawUnits != 0 || ins.RawUnitType != "fallback" {
		t.Fatalf("raw_units=%d raw_unit_type=%q, want 0/fallback", ins.RawUnits, ins.RawUnitType)
	}
	if ins.CostMillicents != 321 {
		t.Fatalf("cost=%d, want 321", ins.CostMillicents)
	}
}

func TestMonthlyLLMCostExceeded_MetaStore(t *testing.T) {
	mock := &mockMetaLLMStore{monthly: 5000}
	b := &Dat9Backend{
		metaLLMStore:                mock,
		tenantID:                    "tenant-1",
		maxMonthlyLLMCostMillicents: 4000,
	}
	if !b.monthlyLLMCostExceeded() {
		t.Fatal("expected exceeded, got false")
	}
}

func TestMonthlyLLMCostExceeded_MetaStore_NotExceeded(t *testing.T) {
	mock := &mockMetaLLMStore{monthly: 3000}
	b := &Dat9Backend{
		metaLLMStore:                mock,
		tenantID:                    "tenant-1",
		maxMonthlyLLMCostMillicents: 4000,
	}
	if b.monthlyLLMCostExceeded() {
		t.Fatal("expected not exceeded, got true")
	}
}

func TestMonthlyLLMCostExceeded_MetaStoreFailure_FailOpen(t *testing.T) {
	mock := &mockMetaLLMStore{queryErr: errors.New("meta db down")}
	b := &Dat9Backend{
		metaLLMStore:                mock,
		tenantID:                    "tenant-1",
		maxMonthlyLLMCostMillicents: 4000,
	}
	if b.monthlyLLMCostExceeded() {
		t.Fatal("expected fail-open (false), got true")
	}
}

func TestMonthlyLLMCostExceeded_ServerQuota_NotExceeded(t *testing.T) {
	mock := &mockQuotaStore{monthlyCost: 3000}
	b := &Dat9Backend{
		metaStore:                   mock,
		tenantID:                    "tenant-1",
		quotaSource:                 QuotaSourceServer,
		maxMonthlyLLMCostMillicents: 4000,
	}
	if b.monthlyLLMCostExceeded() {
		t.Fatal("expected not exceeded, got true")
	}
}

func TestMonthlyLLMCostExceeded_ServerQuota_FailOpen(t *testing.T) {
	mock := &mockQuotaStore{monthlyCostErr: errors.New("meta db down")}
	b := &Dat9Backend{
		metaStore:                   mock,
		tenantID:                    "tenant-1",
		quotaSource:                 QuotaSourceServer,
		maxMonthlyLLMCostMillicents: 4000,
	}
	if b.monthlyLLMCostExceeded() {
		t.Fatal("expected fail-open (false), got true")
	}
}

func TestMonthlyLLMCostExceeded_DisabledBudget(t *testing.T) {
	b := &Dat9Backend{
		maxMonthlyLLMCostMillicents: 0,
	}
	if b.monthlyLLMCostExceeded() {
		t.Fatal("expected false when budget disabled")
	}
}

func TestMonthlyLLMCostExceeded_DualRead(t *testing.T) {
	mock := &mockMetaLLMStore{monthly: 2000}
	store := newTestStore(t)
	if err := store.InsertLLMUsage("img_extract_text", "task-1", 2500, 100, "tokens"); err != nil {
		t.Fatal(err)
	}

	b := &Dat9Backend{
		store:                       store,
		metaLLMStore:                mock,
		tenantID:                    "tenant-1",
		maxMonthlyLLMCostMillicents: 4000,
		llmUsageDualRead:            true,
	}
	if !b.monthlyLLMCostExceeded() {
		t.Fatal("expected exceeded with dual-read (2000+2500=4500 > 4000), got false")
	}
}

func TestMonthlyLLMCostExceeded_DualRead_NotExceeded(t *testing.T) {
	mock := &mockMetaLLMStore{monthly: 1000}
	store := newTestStore(t)
	if err := store.InsertLLMUsage("img_extract_text", "task-1", 1000, 50, "tokens"); err != nil {
		t.Fatal(err)
	}

	b := &Dat9Backend{
		store:                       store,
		metaLLMStore:                mock,
		tenantID:                    "tenant-1",
		maxMonthlyLLMCostMillicents: 4000,
		llmUsageDualRead:            true,
	}
	if b.monthlyLLMCostExceeded() {
		t.Fatal("expected not exceeded with dual-read (1000+1000=2000 < 4000), got true")
	}
}

func TestMonthlyLLMCostExceeded_DualRead_TenantStoreFailure(t *testing.T) {
	mock := &mockMetaLLMStore{monthly: 3000}
	store := newTestStore(t)
	_ = store.Close()

	b := &Dat9Backend{
		store:                       store,
		metaLLMStore:                mock,
		tenantID:                    "tenant-1",
		maxMonthlyLLMCostMillicents: 4000,
		llmUsageDualRead:            true,
	}
	if b.monthlyLLMCostExceeded() {
		t.Fatal("expected not exceeded (meta-only 3000 < 4000), got true")
	}
}

func newTestStore(t *testing.T) *datastore.Store {
	t.Helper()
	if testDSN == "" {
		t.Skip("test MySQL DSN not available")
	}
	store, err := datastore.Open(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, _ = store.DB().Exec("DELETE FROM llm_usage")
	return store
}
