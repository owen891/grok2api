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
	"strings"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
)

const maxCredentialBytes = 1 << 20

type credentialImporter interface {
	ImportCredentials(context.Context, []byte) (accountapp.ImportResult, error)
	ImportWebCredentials(context.Context, []byte) (accountapp.ImportResult, error)
}

type accountSynchronizer interface {
	Sync(context.Context, ...uint64) accountsyncapp.Result
}

type Config struct {
	Enabled      bool
	SpoolPath    string
	PollInterval time.Duration
	WorkDir      string
	ConfigPath   string
	Command      []string
	BrowserMode  string
	BrowserPath  string
}

type Service struct {
	logger   *slog.Logger
	importer credentialImporter
	syncer   accountSynchronizer
	config   Config
}

type fileResult struct {
	Status      string    `json:"status"`
	Created     int       `json:"created"`
	Updated     int       `json:"updated"`
	Synced      int       `json:"synced"`
	SyncFailed  int       `json:"syncFailed"`
	ProcessedAt time.Time `json:"processedAt"`
}

func NewService(logger *slog.Logger, importer credentialImporter, syncer accountSynchronizer, config Config) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{logger: logger, importer: importer, syncer: syncer, config: config}
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
	incoming, processed, failed, err := s.ensureDirectories()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(incoming)
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
		source := filepath.Join(incoming, entry.Name())
		if err := s.processFile(ctx, source, processed, failed); err != nil {
			s.logger.Error("registration_spool_file_failed", "file", entry.Name(), "error", err)
		}
	}
	return nil
}

func (s *Service) ensureDirectories() (string, string, string, error) {
	paths := []string{
		filepath.Join(s.config.SpoolPath, "incoming"),
		filepath.Join(s.config.SpoolPath, "processed"),
		filepath.Join(s.config.SpoolPath, "failed"),
	}
	for _, path := range paths {
		if err := ensurePrivateDirectory(path); err != nil {
			return "", "", "", fmt.Errorf("创建注册凭据目录 %s: %w", path, err)
		}
	}
	return paths[0], paths[1], paths[2], nil
}

func (s *Service) processFile(ctx context.Context, source, processed, failed string) error {
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查注册凭据文件: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return s.finish(source, failed, fileResult{Status: "rejected"})
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if info.Size() == 0 || info.Size() > maxCredentialBytes {
		return s.finish(source, failed, fileResult{Status: "rejected"})
	}

	data, err := readCredentialFile(source, info)
	if err != nil {
		return s.finish(source, failed, fileResult{Status: "rejected"})
	}
	imported, err := s.importCredential(ctx, data)
	result := fileResult{Created: imported.Created, Updated: imported.Updated}
	if err != nil {
		result.Status = "import_failed"
		return s.finish(source, failed, result)
	}

	synced := s.syncer.Sync(ctx, imported.AccountIDs...)
	result.Synced = synced.Succeeded
	result.SyncFailed = synced.Failed
	if synced.Failed > 0 {
		result.Status = "sync_failed"
		return s.finish(source, failed, result)
	}
	result.Status = "processed"
	return s.finish(source, processed, result)
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

func (s *Service) finish(source, destinationDir string, result fileResult) error {
	destination := uniqueDestination(destinationDir, filepath.Base(source))
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
