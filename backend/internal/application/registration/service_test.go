package registration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
)

type importerStub struct {
	result   accountapp.ImportResult
	err      error
	calls    int
	webCalls int
}

type blockingImporter struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (s *blockingImporter) ImportCredentials(context.Context, []byte) (accountapp.ImportResult, error) {
	if s.calls.Add(1) == 1 {
		close(s.entered)
	}
	<-s.release
	return accountapp.ImportResult{Created: 1, AccountIDs: []uint64{42}}, nil
}

func (s *blockingImporter) ImportWebCredentials(context.Context, []byte) (accountapp.ImportResult, error) {
	return s.ImportCredentials(context.Background(), nil)
}

func (s *importerStub) ImportCredentials(context.Context, []byte) (accountapp.ImportResult, error) {
	s.calls++
	return s.result, s.err
}

func (s *importerStub) ImportWebCredentials(context.Context, []byte) (accountapp.ImportResult, error) {
	s.webCalls++
	return s.result, s.err
}

type syncerStub struct {
	result accountsyncapp.Result
	ids    []uint64
}

type nsfwStub struct {
	ids     []uint64
	enabled bool
	succeed int
	failed  int
	err     error
}

func (s *nsfwStub) BatchSetNSFW(_ context.Context, ids []uint64, enabled bool) (int, int, error) {
	s.ids = append([]uint64(nil), ids...)
	s.enabled = enabled
	return s.succeed, s.failed, s.err
}

func (s *syncerStub) Sync(_ context.Context, ids ...uint64) accountsyncapp.Result {
	s.ids = append([]uint64(nil), ids...)
	return s.result
}

func TestProcessOnceMovesSuccessfulCredentialToProcessed(t *testing.T) {
	root := t.TempDir()
	importer := &importerStub{result: accountapp.ImportResult{Created: 1, AccountIDs: []uint64{42}}}
	syncer := &syncerStub{result: accountsyncapp.Result{Succeeded: 1}}
	service := newTestService(root, importer, syncer)
	writeIncoming(t, root, "account.json", []byte(`{"refresh_token":"synthetic"}`))

	if err := service.processOnce(context.Background()); err != nil {
		t.Fatalf("processOnce() error = %v", err)
	}
	assertExists(t, filepath.Join(root, "processed", "account.json"))
	result := readResult(t, filepath.Join(root, "processed", "account.result.json"))
	if result.Status != "processed" || result.Created != 1 || result.Synced != 1 || result.SyncFailed != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(syncer.ids) != 1 || syncer.ids[0] != 42 {
		t.Fatalf("Sync() ids = %v", syncer.ids)
	}
}

func TestProcessOnceRoutesWebCredentialToWebImporter(t *testing.T) {
	root := t.TempDir()
	importer := &importerStub{result: accountapp.ImportResult{Created: 1, AccountIDs: []uint64{43}}}
	syncer := &syncerStub{result: accountsyncapp.Result{Succeeded: 1}}
	service := newTestService(root, importer, syncer)
	writeIncoming(t, root, "web.json", []byte(`{"provider":"grok_web","accounts":[{"sso_token":"synthetic"}]}`))

	if err := service.processOnce(context.Background()); err != nil {
		t.Fatalf("processOnce() error = %v", err)
	}
	if importer.webCalls != 1 || importer.calls != 0 {
		t.Fatalf("web calls = %d, build calls = %d", importer.webCalls, importer.calls)
	}
	assertExists(t, filepath.Join(root, "processed", "web.json"))
}

func TestProcessOnceEnablesNSFWAfterWebImportAndSync(t *testing.T) {
	root := t.TempDir()
	importer := &importerStub{result: accountapp.ImportResult{Created: 1, AccountIDs: []uint64{43}}}
	syncer := &syncerStub{result: accountsyncapp.Result{Succeeded: 1}}
	nsfw := &nsfwStub{succeed: 1}
	service := NewService(slog.Default(), importer, syncer, Config{SpoolPath: root, PollInterval: time.Second, FailedRetention: time.Hour}, nsfw)
	writeIncoming(t, root, "web.json", []byte(`{"provider":"grok_web","auto_nsfw":true,"accounts":[{"sso_token":"synthetic"}]}`))

	if err := service.processOnce(context.Background()); err != nil {
		t.Fatalf("processOnce() error = %v", err)
	}
	result := readResult(t, filepath.Join(root, "processed", "web.result.json"))
	if result.Status != "processed" || !result.NSFWRequested || result.NSFWEnabled != 1 || result.NSFWFailed != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(nsfw.ids) != 1 || nsfw.ids[0] != 43 || !nsfw.enabled {
		t.Fatalf("NSFW call = ids=%v enabled=%v", nsfw.ids, nsfw.enabled)
	}
}

func TestProcessOnceMovesInitialSyncFailureToFailed(t *testing.T) {
	root := t.TempDir()
	importer := &importerStub{result: accountapp.ImportResult{Updated: 1, AccountIDs: []uint64{7}}}
	syncer := &syncerStub{result: accountsyncapp.Result{Failed: 1}}
	service := newTestService(root, importer, syncer)
	writeIncoming(t, root, "account.json", []byte(`{"refresh_token":"synthetic"}`))

	if err := service.processOnce(context.Background()); err != nil {
		t.Fatalf("processOnce() error = %v", err)
	}
	assertExists(t, filepath.Join(root, "failed", "account.json"))
	result := readResult(t, filepath.Join(root, "failed", "account.result.json"))
	if result.Status != "sync_failed" || result.Updated != 1 || result.Synced != 0 || result.SyncFailed != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestProcessOncePersistsInitialSyncFailureDetails(t *testing.T) {
	root := t.TempDir()
	importer := &importerStub{result: accountapp.ImportResult{Created: 1, AccountIDs: []uint64{7}}}
	syncer := &syncerStub{result: accountsyncapp.Result{Failed: 1, Failures: []accountsyncapp.Failure{{AccountID: 7, Error: "Grok Web browser worker unavailable"}}}}
	service := newTestService(root, importer, syncer)
	writeIncoming(t, root, "account.json", []byte(`{"provider":"grok_web"}`))

	if err := service.processOnce(context.Background()); err != nil {
		t.Fatalf("processOnce() error = %v", err)
	}
	result := readResult(t, filepath.Join(root, "failed", "account.result.json"))
	if len(result.SyncErrors) != 1 || result.SyncErrors[0].AccountID != 7 || result.SyncErrors[0].Error == "" {
		t.Fatalf("sync errors = %+v", result.SyncErrors)
	}
}

func TestProcessOnceClaimsCredentialAcrossServiceInstances(t *testing.T) {
	root := t.TempDir()
	importer := &blockingImporter{entered: make(chan struct{}), release: make(chan struct{})}
	syncer := &syncerStub{result: accountsyncapp.Result{Succeeded: 1}}
	first := newTestService(root, importer, syncer)
	second := newTestService(root, importer, syncer)
	writeIncoming(t, root, "account.json", []byte(`{"refresh_token":"synthetic"}`))

	firstDone := make(chan error, 1)
	go func() { firstDone <- first.processOnce(context.Background()) }()
	<-importer.entered
	if err := second.processOnce(context.Background()); err != nil {
		t.Fatalf("second processOnce() error = %v", err)
	}
	close(importer.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first processOnce() error = %v", err)
	}
	if calls := importer.calls.Load(); calls != 1 {
		t.Fatalf("import calls = %d, want 1", calls)
	}
	assertExists(t, filepath.Join(root, "processed", "account.json"))
}

func TestProcessOnceRecoversStaleClaim(t *testing.T) {
	root := t.TempDir()
	importer := &importerStub{result: accountapp.ImportResult{Created: 1, AccountIDs: []uint64{42}}}
	syncer := &syncerStub{result: accountsyncapp.Result{Succeeded: 1}}
	service := newTestService(root, importer, syncer)
	directories, err := service.ensureDirectories()
	if err != nil {
		t.Fatal(err)
	}
	claimedAt := time.Now().Add(-processingRecoveryAge - time.Minute)
	claimName := "account.json" + processingMarker + strconv.FormatInt(claimedAt.UnixNano(), 10) + "-999999"
	if err := os.WriteFile(filepath.Join(directories.processing, claimName), []byte(`{"refresh_token":"synthetic"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := service.processOnce(context.Background()); err != nil {
		t.Fatalf("processOnce() error = %v", err)
	}
	if importer.calls != 1 {
		t.Fatalf("import calls = %d, want 1", importer.calls)
	}
	assertExists(t, filepath.Join(root, "processed", "account.json"))
}

func TestProcessOnceRejectsOversizedCredential(t *testing.T) {
	root := t.TempDir()
	importer := &importerStub{}
	syncer := &syncerStub{}
	service := newTestService(root, importer, syncer)
	writeIncoming(t, root, "large.json", make([]byte, maxCredentialBytes+1))

	if err := service.processOnce(context.Background()); err != nil {
		t.Fatalf("processOnce() error = %v", err)
	}
	assertExists(t, filepath.Join(root, "failed", "large.json"))
	result := readResult(t, filepath.Join(root, "failed", "large.result.json"))
	if result.Status != "rejected" || importer.calls != 0 {
		t.Fatalf("result = %+v, importer calls = %d", result, importer.calls)
	}
}

func TestEnsureDirectoriesTightensExistingDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX directory permissions")
	}
	root := t.TempDir()
	incoming := filepath.Join(root, "incoming")
	if err := os.MkdirAll(incoming, 0o755); err != nil {
		t.Fatal(err)
	}
	service := newTestService(root, &importerStub{}, &syncerStub{})
	if _, err := service.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		incoming,
		filepath.Join(root, "processing"),
		filepath.Join(root, "processed"),
		filepath.Join(root, "failed"),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("directory %s remains group/world accessible: %o", path, info.Mode().Perm())
		}
	}
}

func TestCleanupFailedRemovesOnlyExpiredJSONFiles(t *testing.T) {
	root := t.TempDir()
	service := newTestService(root, &importerStub{}, &syncerStub{})
	service.config.FailedRetention = time.Hour
	directories, err := service.ensureDirectories()
	if err != nil {
		t.Fatal(err)
	}
	failed := directories.failed
	oldCredential := filepath.Join(failed, "old.json")
	oldResult := filepath.Join(failed, "old.result.json")
	recentCredential := filepath.Join(failed, "recent.json")
	ignored := filepath.Join(failed, "notes.txt")
	for _, path := range []string{oldCredential, oldResult, recentCredential, ignored} {
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	oldTime := now.Add(-2 * time.Hour)
	for _, path := range []string{oldCredential, oldResult, ignored} {
		if err := os.Chtimes(path, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := service.cleanupFailed(failed, now)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	for _, path := range []string{oldCredential, oldResult} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expired file %s still exists: %v", path, err)
		}
	}
	for _, path := range []string{recentCredential, ignored} {
		assertExists(t, path)
	}
}

func newTestService(root string, importer credentialImporter, syncer accountSynchronizer) *Service {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewService(logger, importer, syncer, Config{SpoolPath: root, PollInterval: time.Second, FailedRetention: 7 * 24 * time.Hour})
}

func writeIncoming(t *testing.T, root, name string, data []byte) {
	t.Helper()
	directory := filepath.Join(root, "incoming")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readResult(t *testing.T, path string) fileResult {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result fileResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.ProcessedAt.IsZero() {
		t.Fatal("processedAt is zero")
	}
	return result
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}
