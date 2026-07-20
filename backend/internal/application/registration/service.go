package registration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	accountapp "github.com/owen891/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/owen891/grok2api/backend/internal/application/accountsync"
)

const (
	maxCredentialBytes    = 1 << 20
	processingMarker      = ".processing-"
	processingRecoveryAge = time.Hour
)

type credentialImporter interface {
	ImportCredentials(context.Context, []byte) (accountapp.ImportResult, error)
	ImportWebCredentials(context.Context, []byte) (accountapp.ImportResult, error)
}

type accountSynchronizer interface {
	Sync(context.Context, ...uint64) accountsyncapp.Result
}

type nsfwSetter interface {
	BatchSetNSFW(context.Context, []uint64, bool) (int, int, error)
}

type Config struct {
	Enabled           bool
	SpoolPath         string
	PollInterval      time.Duration
	FailedRetention   time.Duration
	WorkDir           string
	ConfigPath        string
	Command           []string
	BrowserMode       string
	BrowserPath       string
	ResolveProxyGroup func(context.Context, uint64, string) ([]string, error)
}

type Service struct {
	logger   *slog.Logger
	importer credentialImporter
	syncer   accountSynchronizer
	nsfw     nsfwSetter
	config   Config
}

type fileResult struct {
	Status        string      `json:"status"`
	Created       int         `json:"created"`
	Updated       int         `json:"updated"`
	Synced        int         `json:"synced"`
	SyncFailed    int         `json:"syncFailed"`
	SyncErrors    []syncError `json:"syncErrors,omitempty"`
	NSFWRequested bool        `json:"nsfwRequested,omitempty"`
	NSFWEnabled   int         `json:"nsfwEnabled,omitempty"`
	NSFWFailed    int         `json:"nsfwFailed,omitempty"`
	ProcessedAt   time.Time   `json:"processedAt"`
}

type syncError struct {
	AccountID uint64 `json:"accountId"`
	Error     string `json:"error"`
}

type spoolDirectories struct {
	incoming   string
	processing string
	processed  string
	failed     string
}

func NewService(logger *slog.Logger, importer credentialImporter, syncer accountSynchronizer, config Config, nsfw ...nsfwSetter) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	var setter nsfwSetter
	if len(nsfw) > 0 {
		setter = nsfw[0]
	}
	return &Service{logger: logger, importer: importer, syncer: syncer, nsfw: setter, config: config}
}

func (s *Service) Run(ctx context.Context) error {
	if err := s.processOnce(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(s.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.processOnce(ctx); err != nil {
				return err
			}
		}
	}
}

func (s *Service) processOnce(ctx context.Context) error {
	directories, err := s.ensureDirectories()
	if err != nil {
		return err
	}
	now := time.Now()
	removed, cleanupErr := s.cleanupFailed(directories.failed, now)
	if cleanupErr != nil {
		s.logger.Warn("registration_spool_failed_cleanup_partial", "error", cleanupErr)
	}
	if removed > 0 {
		s.logger.Info("registration_spool_failed_cleanup_completed", "removed", removed)
	}
	if err := s.processStaleClaims(ctx, directories, now); err != nil {
		return err
	}
	entries, err := os.ReadDir(directories.incoming)
	if err != nil {
		return fmt.Errorf("读取注册凭据目录: %w", err)
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			continue
		}
		source := filepath.Join(directories.incoming, entry.Name())
		claimed, ok, claimErr := claimFile(source, directories.processing, entry.Name(), now)
		if claimErr != nil {
			s.logger.Error("registration_spool_file_claim_failed", "file", entry.Name(), "error", claimErr)
			continue
		}
		if !ok {
			continue
		}
		if err := s.processFile(ctx, claimed, entry.Name(), directories.processed, directories.failed); err != nil {
			s.logger.Error("registration_spool_file_failed", "file", entry.Name(), "error", err)
		}
	}
	return nil
}

func (s *Service) ensureDirectories() (spoolDirectories, error) {
	directories := spoolDirectories{
		incoming:   filepath.Join(s.config.SpoolPath, "incoming"),
		processing: filepath.Join(s.config.SpoolPath, "processing"),
		processed:  filepath.Join(s.config.SpoolPath, "processed"),
		failed:     filepath.Join(s.config.SpoolPath, "failed"),
	}
	for _, path := range []string{directories.incoming, directories.processing, directories.processed, directories.failed} {
		if err := ensurePrivateDirectory(path); err != nil {
			return spoolDirectories{}, fmt.Errorf("创建注册凭据目录 %s: %w", path, err)
		}
	}
	return directories, nil
}

func (s *Service) processStaleClaims(ctx context.Context, directories spoolDirectories, now time.Time) error {
	entries, err := os.ReadDir(directories.processing)
	if err != nil {
		return fmt.Errorf("read registration processing spool: %w", err)
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return nil
		}
		originalName, claimedAt, ok := parseClaimName(entry.Name())
		if !ok || now.Sub(claimedAt) < processingRecoveryAge {
			continue
		}
		source := filepath.Join(directories.processing, entry.Name())
		claimed, reclaimed, claimErr := claimFile(source, directories.processing, originalName, now)
		if claimErr != nil {
			s.logger.Error("registration_spool_file_reclaim_failed", "file", originalName, "error", claimErr)
			continue
		}
		if !reclaimed {
			continue
		}
		s.logger.Warn("registration_spool_file_reclaimed", "file", originalName)
		if err := s.processFile(ctx, claimed, originalName, directories.processed, directories.failed); err != nil {
			s.logger.Error("registration_spool_file_failed", "file", originalName, "error", err)
		}
	}
	return nil
}

func claimFile(source, processingDirectory, originalName string, now time.Time) (string, bool, error) {
	claimed := filepath.Join(processingDirectory, fmt.Sprintf("%s%s%d-%d", originalName, processingMarker, now.UnixNano(), os.Getpid()))
	if err := os.Rename(source, claimed); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("claim registration spool file: %w", err)
	}
	return claimed, true, nil
}

func parseClaimName(name string) (string, time.Time, bool) {
	marker := strings.LastIndex(name, processingMarker)
	if marker <= 0 {
		return "", time.Time{}, false
	}
	originalName := name[:marker]
	if !strings.EqualFold(filepath.Ext(originalName), ".json") {
		return "", time.Time{}, false
	}
	tail := name[marker+len(processingMarker):]
	separator := strings.IndexByte(tail, '-')
	if separator <= 0 {
		return "", time.Time{}, false
	}
	nanoseconds, err := strconv.ParseInt(tail[:separator], 10, 64)
	if err != nil || nanoseconds <= 0 {
		return "", time.Time{}, false
	}
	return originalName, time.Unix(0, nanoseconds), true
}

func (s *Service) cleanupFailed(directory string, now time.Time) (int, error) {
	if s.config.FailedRetention <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return 0, fmt.Errorf("read failed registration spool: %w", err)
	}
	cutoff := now.Add(-s.config.FailedRetention)
	removed := 0
	var cleanupErr error
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("inspect failed registration spool file %s: %w", entry.Name(), infoErr))
			continue
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		if removeErr := os.Remove(filepath.Join(directory, entry.Name())); removeErr != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove expired registration spool file %s: %w", entry.Name(), removeErr))
			continue
		}
		removed++
	}
	return removed, cleanupErr
}

func (s *Service) processFile(ctx context.Context, source, originalName, processed, failed string) error {
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查注册凭据文件: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return s.finish(source, originalName, failed, fileResult{Status: "rejected"})
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if info.Size() == 0 || info.Size() > maxCredentialBytes {
		return s.finish(source, originalName, failed, fileResult{Status: "rejected"})
	}

	data, err := readCredentialFile(source, info)
	if err != nil {
		return s.finish(source, originalName, failed, fileResult{Status: "rejected"})
	}
	var envelope struct {
		AutoNSFW bool `json:"auto_nsfw"`
	}
	_ = json.Unmarshal(data, &envelope)
	imported, err := s.importCredential(ctx, data)
	result := fileResult{Created: imported.Created, Updated: imported.Updated, NSFWRequested: envelope.AutoNSFW}
	if err != nil {
		result.Status = "import_failed"
		return s.finish(source, originalName, failed, result)
	}

	synced := s.syncer.Sync(ctx, imported.AccountIDs...)
	result.Synced = synced.Succeeded
	result.SyncFailed = synced.Failed
	for _, failure := range synced.Failures {
		result.SyncErrors = append(result.SyncErrors, syncError{AccountID: failure.AccountID, Error: failure.Error})
	}
	if synced.Failed > 0 {
		result.Status = "sync_failed"
		return s.finish(source, originalName, failed, result)
	}
	if envelope.AutoNSFW && len(imported.AccountIDs) > 0 {
		if s.nsfw == nil {
			result.NSFWFailed = len(imported.AccountIDs)
			result.Status = "nsfw_failed"
			return s.finish(source, originalName, failed, result)
		}
		succeeded, failedCount, nsfwErr := s.nsfw.BatchSetNSFW(ctx, imported.AccountIDs, true)
		result.NSFWEnabled = succeeded
		result.NSFWFailed = failedCount
		if nsfwErr != nil || failedCount > 0 {
			result.Status = "nsfw_failed"
			return s.finish(source, originalName, failed, result)
		}
	}
	result.Status = "processed"
	return s.finish(source, originalName, processed, result)
}

func (s *Service) importCredential(ctx context.Context, data []byte) (accountapp.ImportResult, error) {
	var envelope struct {
		Provider string `json:"provider"`
	}
	if json.Unmarshal(data, &envelope) == nil && strings.EqualFold(strings.TrimSpace(envelope.Provider), "grok_web") {
		return s.importer.ImportWebCredentials(ctx, data)
	}
	return s.importer.ImportCredentials(ctx, data)
}

func readCredentialFile(path string, expected os.FileInfo) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(expected, opened) {
		return nil, errors.New("注册凭据文件在读取前发生变化")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxCredentialBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > maxCredentialBytes {
		return nil, errors.New("注册凭据文件大小无效")
	}
	return data, nil
}

func (s *Service) finish(source, originalName, destinationDir string, result fileResult) error {
	destination := uniqueDestination(destinationDir, originalName)
	if err := os.Rename(source, destination); err != nil {
		return fmt.Errorf("移动注册凭据文件: %w", err)
	}
	result.ProcessedAt = time.Now().UTC()
	resultName := strings.TrimSuffix(filepath.Base(destination), filepath.Ext(destination)) + ".result.json"
	if err := writeResult(filepath.Join(destinationDir, resultName), result); err != nil {
		if rollbackErr := os.Rename(destination, source); rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("回滚注册凭据文件: %w", rollbackErr))
		}
		return err
	}
	s.logger.Info("registration_spool_file_processed", "file", filepath.Base(destination), "status", result.Status, "created", result.Created, "updated", result.Updated, "synced", result.Synced, "sync_failed", result.SyncFailed)
	return nil
}

func uniqueDestination(directory, name string) string {
	candidate := filepath.Join(directory, name)
	if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
		return candidate
	}
	extension := filepath.Ext(name)
	stem := strings.TrimSuffix(name, extension)
	return filepath.Join(directory, fmt.Sprintf("%s-%d%s", stem, time.Now().UTC().UnixNano(), extension))
}

func writeResult(path string, result fileResult) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("编码注册导入结果: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".registration-result-*.tmp")
	if err != nil {
		return fmt.Errorf("创建注册导入结果: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("保护注册导入结果: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("写入注册导入结果: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭注册导入结果: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("提交注册导入结果: %w", err)
	}
	return nil
}
