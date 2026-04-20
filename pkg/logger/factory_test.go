package logger

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestNewCLILoggerCreatesLogDirAndFile(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	logPath, err := CLILogPath()
	if err != nil {
		t.Fatalf("CLILogPath: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(logPath)); !os.IsNotExist(err) {
		t.Fatalf("expected log dir to be absent before init, got err=%v", err)
	}

	l, err := NewCLILogger()
	if err != nil {
		t.Fatalf("NewCLILogger: %v", err)
	}
	t.Cleanup(func() { _ = l.Sync() })

	info, err := os.Stat(filepath.Dir(logPath))
	if err != nil {
		t.Fatalf("Stat(log dir): %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected log dir, got file: %s", filepath.Dir(logPath))
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("Stat(log file): %v", err)
	}
}

func TestBenchTimingLogEnabledCachesUntilReset(t *testing.T) {
	resetBenchTimingLogEnabledForTest()
	t.Cleanup(resetBenchTimingLogEnabledForTest)

	t.Setenv(envBenchTimingLogEnabled, "true")
	if !BenchTimingLogEnabled() {
		t.Fatal("expected bench timing log to be enabled")
	}

	t.Setenv(envBenchTimingLogEnabled, "false")
	if !BenchTimingLogEnabled() {
		t.Fatal("expected cached enabled value to remain true before reset")
	}

	resetBenchTimingLogEnabledForTest()
	if BenchTimingLogEnabled() {
		t.Fatal("expected bench timing log to be disabled after reset")
	}
}

func TestInfoBenchTimingHonorsEnabledFlag(t *testing.T) {
	resetBenchTimingLogEnabledForTest()
	t.Cleanup(resetBenchTimingLogEnabledForTest)

	core, recorded := observer.New(zap.InfoLevel)
	ctx := WithContext(context.Background(), zap.New(core))

	t.Setenv(envBenchTimingLogEnabled, "false")
	InfoBenchTiming(ctx, "timing_disabled")
	if entries := recorded.All(); len(entries) != 0 {
		t.Fatalf("recorded %d entries with timing disabled, want 0", len(entries))
	}

	resetBenchTimingLogEnabledForTest()
	t.Setenv(envBenchTimingLogEnabled, "true")
	InfoBenchTiming(ctx, "timing_enabled")
	entries := recorded.All()
	if len(entries) != 1 {
		t.Fatalf("recorded %d entries with timing enabled, want 1", len(entries))
	}
	if entries[0].Message != "timing_enabled" {
		t.Fatalf("message = %q, want timing_enabled", entries[0].Message)
	}
}

func TestHumanDateTimeJSONEncoderAddsDateTimeField(t *testing.T) {
	encoderCfg := zap.NewProductionEncoderConfig()
	encoder := newHumanDateTimeJSONEncoder(encoderCfg)
	entry := zapcore.Entry{
		Level:   zap.InfoLevel,
		Time:    time.Date(2018, 12, 15, 14, 20, 11, 15*1e6, time.FixedZone("+08", 8*60*60)),
		Message: "hello",
	}

	buf, err := encoder.EncodeEntry(entry, nil)
	if err != nil {
		t.Fatalf("EncodeEntry: %v", err)
	}
	defer buf.Free()

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := got["ts"]; !ok {
		t.Fatalf("missing ts field: %s", buf.String())
	}
	if got["date_time"] != "2018/12/15 14:20:11.015 +08:00" {
		t.Fatalf("date_time=%v, want %q", got["date_time"], "2018/12/15 14:20:11.015 +08:00")
	}
}
