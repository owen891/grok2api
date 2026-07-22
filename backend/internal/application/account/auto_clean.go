package account

import (
	"context"
	"fmt"
	"time"

	"github.com/owen891/grok2api/backend/internal/repository"
)

// AutoCleanConfig is deliberately separate from infra/config so the account
// service can be tested and used without depending on YAML configuration.
type AutoCleanConfig struct {
	Enabled         bool
	Interval        time.Duration
	MinAge          time.Duration
	IncludeDisabled bool
}

const (
	autoCleanReauthBatchSize  = 100
	autoCleanMaxScanBatches   = 50
	autoCleanMaxDeleteBatches = 10
	autoCleanLockKey          = "account-auto-clean:reauth"
	autoCleanLockTTL          = 5 * time.Minute
	autoCleanRunTimeout       = 4 * time.Minute
	autoCleanCleanupTimeout   = 3 * time.Second
)

func normalizeAutoCleanConfig(value AutoCleanConfig) AutoCleanConfig {
	if value.Interval < time.Minute {
		value.Interval = time.Minute
	}
	if value.Interval > time.Hour {
		value.Interval = time.Hour
	}
	if value.MinAge < time.Minute {
		value.MinAge = time.Minute
	}
	if value.MinAge > 30*24*time.Hour {
		value.MinAge = 30 * 24 * time.Hour
	}
	return value
}

// UpdateAutoCleanConfig applies a hot-reloaded policy and wakes the scheduler.
func (s *Service) UpdateAutoCleanConfig(value AutoCleanConfig) {
	value = normalizeAutoCleanConfig(value)
	s.autoCleanMu.Lock()
	if s.autoClean == value {
		s.autoCleanMu.Unlock()
		return
	}
	s.autoClean = value
	s.autoCleanRevision++
	s.autoCleanMu.Unlock()
	select {
	case s.autoCleanWake <- struct{}{}:
	default:
	}
}

func (s *Service) autoCleanSnapshot() (AutoCleanConfig, uint64) {
	s.autoCleanMu.RLock()
	defer s.autoCleanMu.RUnlock()
	return s.autoClean, s.autoCleanRevision
}

func autoCleanInterval(value AutoCleanConfig) time.Duration {
	if !value.Enabled {
		return time.Hour
	}
	return normalizeAutoCleanConfig(value).Interval
}

// RunAccountAutoClean periodically removes only aged reauthRequired accounts.
func (s *Service) RunAccountAutoClean(ctx context.Context) {
	value, revision := s.autoCleanSnapshot()
	timer := time.NewTimer(autoCleanInterval(value))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.autoCleanWake:
			value, revision = s.autoCleanSnapshot()
			resetCredentialRefreshTimer(timer, autoCleanInterval(value))
		case <-timer.C:
			current, currentRevision := s.autoCleanSnapshot()
			if current.Enabled && currentRevision == revision {
				if err := s.runAutoCleanReauth(ctx, current, currentRevision); err != nil && ctx.Err() == nil {
					s.logger.Warn("account_auto_clean_failed", "error", err)
				}
			}
			value, revision = s.autoCleanSnapshot()
			resetCredentialRefreshTimer(timer, autoCleanInterval(value))
		}
	}
}

func (s *Service) runAutoCleanReauth(ctx context.Context, value AutoCleanConfig, revision uint64) error {
	if !value.Enabled {
		return nil
	}
	runCtx, cancel := context.WithTimeout(ctx, autoCleanRunTimeout)
	defer cancel()
	if s.refreshLock != nil {
		release, acquired, err := s.refreshLock.Acquire(runCtx, autoCleanLockKey, autoCleanLockTTL)
		if err != nil {
			return err
		}
		if !acquired {
			return nil
		}
		if release != nil {
			defer release()
		}
	}
	markedBefore := s.now().Add(-normalizeAutoCleanConfig(value).MinAge)
	var afterID uint64
	var scanned, deleted, skipped, scanBatches, deleteBatches int
	for scanBatches < autoCleanMaxScanBatches && deleteBatches < autoCleanMaxDeleteBatches {
		current, currentRevision := s.autoCleanSnapshot()
		if currentRevision != revision || current != value {
			return nil
		}
		candidates, err := s.accounts.ListAutoCleanReauthIDs(runCtx, markedBefore, value.IncludeDisabled, afterID, autoCleanReauthBatchSize)
		if err != nil {
			return err
		}
		if len(candidates) == 0 {
			break
		}
		scanBatches++
		scanned += len(candidates)
		afterID = candidates[len(candidates)-1]
		deletable, active, err := s.excludeActiveAccounts(runCtx, candidates)
		if err != nil {
			return err
		}
		skipped += active
		if len(deletable) > 0 {
			ids, ok, err := s.deleteAutoCleanReauth(runCtx, markedBefore, value, revision, deletable)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			deleteBatches++
			deleted += len(ids)
			skipped += len(deletable) - len(ids)
			s.clearAutoCleanRuntimeState(ctx, ids)
		}
		if len(candidates) < autoCleanReauthBatchSize {
			break
		}
	}
	if scanned > 0 {
		s.logger.Info("account_auto_clean", "scanned", scanned, "deleted", deleted, "skipped", skipped, "scan_batches", scanBatches, "delete_batches", deleteBatches)
	}
	return nil
}

// deleteAutoCleanReauth serializes the final policy check with a settings
// update. This prevents a disable/update request from racing a destructive
// transaction that has already passed its snapshot check.
func (s *Service) deleteAutoCleanReauth(ctx context.Context, markedBefore time.Time, value AutoCleanConfig, revision uint64, ids []uint64) ([]uint64, bool, error) {
	s.autoCleanMu.RLock()
	defer s.autoCleanMu.RUnlock()
	if s.autoCleanRevision != revision || s.autoClean != value {
		return nil, false, nil
	}
	deleted, err := s.accounts.DeleteAutoCleanReauthIDs(ctx, markedBefore, value.IncludeDisabled, ids)
	return deleted, true, err
}

func (s *Service) clearAutoCleanRuntimeState(ctx context.Context, ids []uint64) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), autoCleanCleanupTimeout)
	defer cancel()
	for _, id := range ids {
		if s.sticky != nil {
			if err := s.sticky.DeleteByAccount(cleanupCtx, id); err != nil {
				s.logger.Warn("account_auto_clean_runtime_cleanup_failed", "account_id", id, "error", err)
			}
		}
		s.clearRefreshState(id)
	}
}

func (s *Service) excludeActiveAccounts(ctx context.Context, ids []uint64) ([]uint64, int, error) {
	if s.concurrency == nil || len(ids) == 0 {
		return append([]uint64(nil), ids...), 0, nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = repository.AccountConcurrencyKey(id)
	}
	counts := make(map[string]int, len(keys))
	if snapshot, ok := s.concurrency.(repository.ConcurrencySnapshotReader); ok {
		var err error
		counts, err = snapshot.CurrentMany(ctx, keys)
		if err != nil {
			return nil, 0, fmt.Errorf("read account leases: %w", err)
		}
	} else {
		for _, key := range keys {
			current, err := s.concurrency.Current(ctx, key)
			if err != nil {
				return nil, 0, fmt.Errorf("read account lease: %w", err)
			}
			counts[key] = current
		}
	}
	deletable := make([]uint64, 0, len(ids))
	active := 0
	for i, id := range ids {
		if counts[keys[i]] > 0 {
			active++
			continue
		}
		deletable = append(deletable, id)
	}
	return deletable, active, nil
}
