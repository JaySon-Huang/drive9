package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

const envBenchTimingLogEnabled = "DRIVE9_BENCH_TIMING_LOG_ENABLED"
const humanDateTimeLayout = "2006/01/02 15:04:05.000 -07:00"
const drive9JSONEncoderName = "drive9-json"

const (
	benchTimingLogUnknown uint32 = iota
	benchTimingLogDisabled
	benchTimingLogEnabled
)

var benchTimingLogState atomic.Uint32

var (
	drive9JSONEncoderOnce sync.Once
	drive9JSONEncoderErr  error
)

type humanDateTimeEncoder struct {
	zapcore.Encoder
}

func (e humanDateTimeEncoder) Clone() zapcore.Encoder {
	return humanDateTimeEncoder{Encoder: e.Encoder.Clone()}
}

func (e humanDateTimeEncoder) EncodeEntry(ent zapcore.Entry, fields []zapcore.Field) (*buffer.Buffer, error) {
	augmented := make([]zapcore.Field, 0, len(fields)+1)
	augmented = append(augmented, fields...)
	augmented = append(augmented, zap.String("date_time", ent.Time.Format(humanDateTimeLayout)))
	return e.Encoder.EncodeEntry(ent, augmented)
}

func newHumanDateTimeJSONEncoder(cfg zapcore.EncoderConfig) zapcore.Encoder {
	return humanDateTimeEncoder{Encoder: zapcore.NewJSONEncoder(cfg)}
}

func registerDrive9JSONEncoder() error {
	drive9JSONEncoderOnce.Do(func() {
		drive9JSONEncoderErr = zap.RegisterEncoder(
			drive9JSONEncoderName,
			func(cfg zapcore.EncoderConfig) (zapcore.Encoder, error) {
				return newHumanDateTimeJSONEncoder(cfg), nil
			},
		)
	})
	return drive9JSONEncoderErr
}

func NewServerLogger() (*zap.Logger, error) {
	// Register a custom encoder instead of replacing the core with WrapCore so
	// zap's production Build path keeps its default sampling, caller, and
	// stacktrace behavior while we add a human-readable date_time field.
	if err := registerDrive9JSONEncoder(); err != nil {
		return nil, err
	}
	cfg := zap.NewProductionConfig()
	cfg.Encoding = drive9JSONEncoderName
	return cfg.Build()
}

func BenchTimingLogEnabled() bool {
	switch benchTimingLogState.Load() {
	case benchTimingLogDisabled:
		return false
	case benchTimingLogEnabled:
		return true
	}

	enabled := envBool(envBenchTimingLogEnabled, false)
	if enabled {
		benchTimingLogState.Store(benchTimingLogEnabled)
		return true
	}
	benchTimingLogState.Store(benchTimingLogDisabled)
	return false
}

func CLIEnabled() bool {
	return envBool("DRIVE9_CLI_LOG_ENABLED", false)
}

func CLILogDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, ".drive9", "cli"), nil
}

func CLILogPath() (string, error) {
	logDir, err := CLILogDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(logDir, "drive9-cli.log"), nil
}

func NewCLILogger() (*zap.Logger, error) {
	logDir, err := CLILogDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	logPath, err := CLILogPath()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	_ = f.Close()

	rotate := &lumberjack.Logger{
		Filename:   logPath,
		MaxSize:    envInt("DRIVE9_CLI_LOG_MAX_SIZE_MB", 10),
		MaxBackups: envInt("DRIVE9_CLI_LOG_MAX_BACKUPS", 5),
		MaxAge:     envInt("DRIVE9_CLI_LOG_MAX_AGE_DAYS", 14),
		Compress:   true,
	}

	encoderCfg := zap.NewProductionEncoderConfig()
	core := zapcore.NewCore(
		newHumanDateTimeJSONEncoder(encoderCfg),
		zapcore.AddSync(rotate),
		zap.InfoLevel,
	)
	return zap.New(core), nil
}

func envInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func envBool(key string, fallback bool) bool {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return v
}

func resetBenchTimingLogEnabledForTest() {
	benchTimingLogState.Store(benchTimingLogUnknown)
}
