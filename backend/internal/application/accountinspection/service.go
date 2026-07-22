package accountinspection

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	accountapp "github.com/owen891/grok2api/backend/internal/application/account"
	"github.com/owen891/grok2api/backend/internal/application/gateway"
	"github.com/owen891/grok2api/backend/internal/domain/account"
	inspectiondomain "github.com/owen891/grok2api/backend/internal/domain/accountinspection"
	modeldomain "github.com/owen891/grok2api/backend/internal/domain/model"
	"github.com/owen891/grok2api/backend/internal/infra/provider"
	"github.com/owen891/grok2api/backend/internal/observability"
	"github.com/owen891/grok2api/backend/internal/repository"
)

const (
	maxFullTargets       = 5000
	maxSelectedTargets   = 1000
	maxProbeResponseBody = 64 << 10
	inspectionLease      = 2 * time.Minute
	inspectionPoll       = 2 * time.Second
	inspectionHeartbeat  = 20 * time.Second
	// Browser-backed Grok Web probes may spend up to 45 seconds clearing a
	// Cloudflare challenge and another transition through the account TOS gate
	// before the actual chat response arrives. Keep this aligned with the Web
	// adapter's default chat timeout so a slow first session is not misclassified
	// as an upstream network failure.
	inspectionProbeLimit = 120 * time.Second
)

var (
	ErrInvalidInput  = errors.New("巡检参数无效")
	ErrNoTargets     = errors.New("没有符合条件的巡检账号")
	ErrConflict      = errors.New("该 Provider 已有巡检任务运行")
	ErrStaleEvidence = errors.New("账号在巡检后已变化，请复检后再应用")
)

// InvalidInputError preserves the validation reason for transport-specific
// error codes while remaining compatible with errors.Is(ErrInvalidInput).
type InvalidInputError struct {
	Reason string
}

func (e *InvalidInputError) Error() string {
	if e == nil || e.Reason == "" {
		return ErrInvalidInput.Error()
	}
	return fmt.Sprintf("%s: %s", ErrInvalidInput, e.Reason)
}

func (e *InvalidInputError) Unwrap() error { return ErrInvalidInput }

type StartInput struct {
	Provider        account.Provider
	ModelRouteID    uint64
	Mode            inspectiondomain.RunMode
	AccountIDs      []uint64
	Classifications []inspectiondomain.Classification
	IncludeDisabled bool
	Concurrency     int
}

type ApplyResult struct {
	Applied int
	Skipped int
	Failed  int
}

type credentialManager interface {
	MarkReauthRequired(context.Context, uint64, string) error
	MarkInspectionHealthy(context.Context, uint64) (account.Credential, error)
}

type Service struct {
	accounts    repository.AccountRepository
	models      repository.ModelRepository
	runs        repository.AccountInspectionRepository
	providers   *provider.Registry
	credentials credentialManager
	selector    *gateway.Selector
	logger      *slog.Logger
	wake        chan struct{}
	now         func() time.Time
	probeLimit  time.Duration
	lease       time.Duration
	heartbeat   time.Duration
	watchPoll   time.Duration
}

func NewService(accounts repository.AccountRepository, models repository.ModelRepository, runs repository.AccountInspectionRepository, providers *provider.Registry, credentials *accountapp.Service, selector *gateway.Selector, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		accounts: accounts, models: models, runs: runs, providers: providers, credentials: credentials, selector: selector,
		logger: logger, wake: make(chan struct{}, 1), now: func() time.Time { return time.Now().UTC() }, probeLimit: inspectionProbeLimit,
		lease: inspectionLease, heartbeat: inspectionHeartbeat, watchPoll: 2 * time.Second,
	}
}

func (s *Service) Start(ctx context.Context, input StartInput) (inspectiondomain.Run, error) {
	route, err := s.validateStart(ctx, &input)
	if err != nil {
		return inspectiondomain.Run{}, err
	}
	targets, err := s.resolveTargets(ctx, input)
	if err != nil {
		return inspectiondomain.Run{}, err
	}
	if len(targets) == 0 {
		return inspectiondomain.Run{}, ErrNoTargets
	}
	id, err := newInspectionToken()
	if err != nil {
		return inspectiondomain.Run{}, err
	}
	now := s.now()
	run := inspectiondomain.Run{
		ID: id, Provider: input.Provider, ModelRouteID: route.ID, UpstreamModel: route.UpstreamModel,
		Mode: input.Mode, Status: inspectiondomain.RunStatusQueued, IncludeDisabled: input.IncludeDisabled,
		Concurrency: input.Concurrency, Total: len(targets), CreatedAt: now, UpdatedAt: now,
	}
	results := make([]inspectiondomain.Result, 0, len(targets))
	for _, value := range targets {
		results = append(results, inspectiondomain.Result{
			RunID: run.ID, AccountID: value.ID, Provider: value.Provider, AccountName: value.Name, AccountEmail: value.Email,
			AccountEnabled: value.Enabled, AccountUpdatedAt: value.UpdatedAt, Model: route.UpstreamModel, Classification: inspectiondomain.ClassificationPending,
			SuggestedAction: inspectiondomain.ActionKeep, Confidence: inspectiondomain.ConfidenceLow, CreatedAt: now, UpdatedAt: now,
		})
	}
	if err := s.runs.CreateInspectionRun(ctx, run, results); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return inspectiondomain.Run{}, ErrConflict
		}
		return inspectiondomain.Run{}, err
	}
	s.signalWorker()
	s.logger.Info("account_inspection_queued", "run_id", run.ID, "provider", run.Provider, "model", run.UpstreamModel, "targets", run.Total)
	return run, nil
}

func (s *Service) validateStart(ctx context.Context, input *StartInput) (modeldomain.Route, error) {
	if input == nil || !input.Provider.IsValid() || input.ModelRouteID == 0 || s.models == nil || s.runs == nil || s.accounts == nil || s.providers == nil {
		return modeldomain.Route{}, s.invalidStartInput(input, "provider_or_model_missing")
	}
	if input.Mode == "" {
		input.Mode = inspectiondomain.RunModeFull
	}
	switch input.Mode {
	case inspectiondomain.RunModeFull, inspectiondomain.RunModeIncremental:
		if len(input.AccountIDs) > 0 || len(input.Classifications) > 0 {
			return modeldomain.Route{}, s.invalidStartInput(input, "full_mode_cannot_include_account_or_classification_filters")
		}
	case inspectiondomain.RunModeSelected:
		if len(input.AccountIDs) == 0 || len(input.Classifications) > 0 || len(input.AccountIDs) > maxSelectedTargets {
			return modeldomain.Route{}, s.invalidStartInput(input, "selected_mode_requires_1_to_1000_account_ids_only")
		}
	case inspectiondomain.RunModeRecheck:
		if len(input.AccountIDs) > 0 || len(input.Classifications) == 0 {
			return modeldomain.Route{}, s.invalidStartInput(input, "recheck_mode_requires_classifications_without_account_ids")
		}
		for _, value := range input.Classifications {
			if !value.ValidRecheckTarget() {
				return modeldomain.Route{}, s.invalidStartInput(input, "recheck_mode_contains_unsupported_classification")
			}
		}
	default:
		return modeldomain.Route{}, s.invalidStartInput(input, "inspection_mode_is_not_supported")
	}
	if input.Concurrency == 0 {
		input.Concurrency = 4
	}
	if input.Concurrency < 1 || input.Concurrency > 8 {
		return modeldomain.Route{}, s.invalidStartInput(input, "concurrency_must_be_between_1_and_8")
	}
	route, err := s.models.Get(ctx, input.ModelRouteID)
	if err != nil {
		return modeldomain.Route{}, err
	}
	if !route.Enabled || route.Provider != input.Provider || (route.Capability != modeldomain.CapabilityResponses && route.Capability != modeldomain.CapabilityChat) {
		return modeldomain.Route{}, s.invalidStartInput(input, "model_route_is_not_an_enabled_chat_route_for_provider")
	}
	if route.SyncedAccounts > 0 && route.SupportedAccounts == 0 {
		return modeldomain.Route{}, s.invalidStartInput(input, "model_route_has_no_supported_accounts")
	}
	if _, ok := s.providers.Responses(input.Provider); !ok {
		return modeldomain.Route{}, s.invalidStartInput(input, "provider_does_not_support_responses")
	}
	return route, nil
}

func (s *Service) invalidStartInput(input *StartInput, reason string) error {
	providerValue := ""
	modelRouteID := uint64(0)
	mode := ""
	accountCount := 0
	classificationCount := 0
	concurrency := 0
	if input != nil {
		providerValue = string(input.Provider)
		modelRouteID = input.ModelRouteID
		mode = string(input.Mode)
		accountCount = len(input.AccountIDs)
		classificationCount = len(input.Classifications)
		concurrency = input.Concurrency
	}
	s.logger.Warn("account_inspection_invalid_start_input", "reason", reason, "provider", providerValue,
		"model_route_id", modelRouteID, "mode", mode, "account_count", accountCount,
		"classification_count", classificationCount, "concurrency", concurrency)
	return &InvalidInputError{Reason: reason}
}

func (s *Service) resolveTargets(ctx context.Context, input StartInput) ([]account.Credential, error) {
	var ids []uint64
	switch input.Mode {
	case inspectiondomain.RunModeSelected:
		ids = normalizeIDs(input.AccountIDs)
	case inspectiondomain.RunModeRecheck:
		results, err := s.runs.ListLatestInspectionResults(ctx, input.Provider, input.Classifications)
		if err != nil {
			return nil, err
		}
		for _, value := range results {
			ids = append(ids, value.AccountID)
		}
	}
	if ids != nil {
		values := make([]account.Credential, 0, len(ids))
		for _, id := range ids {
			value, err := s.accounts.Get(ctx, id)
			if errors.Is(err, repository.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if value.Provider != input.Provider || (!input.IncludeDisabled && !value.Enabled) {
				continue
			}
			values = append(values, value)
		}
		return values, nil
	}
	values, _, err := s.accounts.List(ctx, repository.AccountListQuery{
		Page:   repository.PageQuery{Offset: 0, Limit: maxFullTargets},
		Filter: repository.AccountListFilter{Provider: string(input.Provider), Now: s.now()},
	})
	if err != nil {
		return nil, err
	}
	known := make(map[uint64]struct{})
	if input.Mode == inspectiondomain.RunModeIncremental {
		latest, latestErr := s.runs.ListLatestInspectionResults(ctx, input.Provider, []inspectiondomain.Classification{
			inspectiondomain.ClassificationHealthy,
			inspectiondomain.ClassificationPermissionDenied,
			inspectiondomain.ClassificationQuotaExhausted,
			inspectiondomain.ClassificationReauth,
			inspectiondomain.ClassificationModelUnavailable,
			inspectiondomain.ClassificationProbeError,
		})
		if latestErr != nil {
			return nil, latestErr
		}
		for _, value := range latest {
			known[value.AccountID] = struct{}{}
		}
	}
	filtered := values[:0]
	for _, value := range values {
		if !input.IncludeDisabled && !value.Enabled {
			continue
		}
		if _, exists := known[value.ID]; exists {
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered, nil
}

func normalizeIDs(values []uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(values))
	result := make([]uint64, 0, len(values))
	for _, value := range values {
		if value == 0 {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func (s *Service) Get(ctx context.Context, id string) (inspectiondomain.Run, error) {
	return s.runs.GetInspectionRun(ctx, strings.TrimSpace(id))
}

func (s *Service) Latest(ctx context.Context, providerValue account.Provider) (inspectiondomain.Run, error) {
	if !providerValue.IsValid() {
		return inspectiondomain.Run{}, ErrInvalidInput
	}
	return s.runs.GetLatestInspectionRun(ctx, providerValue)
}

func (s *Service) List(ctx context.Context, providerValue account.Provider, limit int) ([]inspectiondomain.Run, error) {
	if providerValue != "" && !providerValue.IsValid() {
		return nil, ErrInvalidInput
	}
	return s.runs.ListInspectionRuns(ctx, providerValue, limit)
}

func (s *Service) Results(ctx context.Context, runID string, page, pageSize int) ([]inspectiondomain.Result, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 500 {
		pageSize = 100
	}
	return s.runs.ListInspectionResults(ctx, strings.TrimSpace(runID), (page-1)*pageSize, pageSize)
}

func (s *Service) ResultSummary(ctx context.Context, runID string) (map[inspectiondomain.Classification]int, error) {
	return s.runs.SummarizeInspectionResults(ctx, strings.TrimSpace(runID))
}

func (s *Service) Cancel(ctx context.Context, id string) (inspectiondomain.Run, error) {
	value, err := s.runs.RequestInspectionCancellation(ctx, strings.TrimSpace(id), s.now())
	if err == nil {
		s.signalWorker()
	}
	return value, err
}

func (s *Service) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.wake:
		case <-timer.C:
		}
		if err := s.processAvailable(ctx); err != nil {
			return err
		}
		timer.Reset(inspectionPoll)
	}
}

func (s *Service) signalWorker() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) processAvailable(ctx context.Context) error {
	for {
		now := s.now()
		ids, err := s.runs.ListClaimableInspectionRunIDs(ctx, now, 10)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		claimedAny := false
		for _, id := range ids {
			token, tokenErr := newInspectionToken()
			if tokenErr != nil {
				return tokenErr
			}
			run, claimed, claimErr := s.runs.TryClaimInspectionRun(ctx, id, token, now, now.Add(s.lease))
			if claimErr != nil {
				return claimErr
			}
			if !claimed {
				continue
			}
			claimedAny = true
			if err := s.processRun(ctx, run); err != nil {
				return err
			}
		}
		if !claimedAny {
			return nil
		}
	}
}

func (s *Service) processRun(parent context.Context, run inspectiondomain.Run) error {
	started := s.now()
	observability.SetAccountInspectionActive(string(run.Provider), true)
	defer observability.SetAccountInspectionActive(string(run.Provider), false)
	pending, err := s.runs.ListPendingInspectionResults(parent, run.ID, maxFullTargets)
	if err != nil {
		return err
	}
	if run.CancelRequested {
		now := s.now()
		if _, err := s.runs.CancelPendingInspectionResults(parent, run.ID, run.ClaimToken, now); err != nil {
			return err
		}
		_, err := s.runs.FinishInspectionRun(parent, run.ID, run.ClaimToken, inspectiondomain.RunStatusCancelled, "", now)
		return err
	}
	workCtx, cancel := context.WithCancel(parent)
	defer cancel()
	var cancelled atomic.Bool
	var leaseLost atomic.Bool
	watchDone := make(chan struct{})
	go s.watchRun(workCtx, run, cancel, &cancelled, &leaseLost, watchDone)

	jobs := make(chan inspectiondomain.Result)
	var workers sync.WaitGroup
	var firstErr error
	var errorMu sync.Mutex
	setError := func(value error) {
		if value == nil {
			return
		}
		errorMu.Lock()
		if firstErr == nil {
			firstErr = value
			cancel()
		}
		errorMu.Unlock()
	}
	for worker := 0; worker < run.Concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for target := range jobs {
				if workCtx.Err() != nil {
					return
				}
				result := s.inspectTarget(workCtx, run, target)
				updated, completeErr := s.runs.CompleteInspectionResult(context.WithoutCancel(workCtx), result, run.ClaimToken, s.now())
				if completeErr != nil {
					setError(completeErr)
					return
				}
				if !updated {
					leaseLost.Store(true)
					cancel()
					return
				}
				observability.ObserveAccountInspectionResult(string(run.Provider), string(result.Classification))
			}
		}()
	}
	producerDone := false
	for _, target := range pending {
		select {
		case <-workCtx.Done():
			producerDone = true
		case jobs <- target:
		}
		if producerDone {
			break
		}
	}
	close(jobs)
	workers.Wait()
	if parent.Err() != nil {
		cancel()
		<-watchDone
		return nil
	}
	if leaseLost.Load() {
		cancel()
		<-watchDone
		if firstErr != nil {
			return firstErr
		}
		return nil
	}
	if firstErr != nil {
		cancel()
		<-watchDone
		return firstErr
	}
	autoResult := ApplyResult{}
	var applyErr error
	if !cancelled.Load() {
		autoResult, applyErr = s.applyRunResults(workCtx, run)
	}
	cancel()
	<-watchDone
	if parent.Err() != nil {
		return nil
	}
	if leaseLost.Load() {
		return nil
	}
	now := s.now()
	status := inspectiondomain.RunStatusCompleted
	message := ""
	if !cancelled.Load() {
		requested, cancelErr := s.runs.InspectionCancellationRequested(parent, run.ID, run.ClaimToken)
		if cancelErr != nil && !errors.Is(cancelErr, repository.ErrNotFound) {
			return cancelErr
		}
		cancelled.Store(requested)
	}
	if cancelled.Load() {
		if _, err := s.runs.CancelPendingInspectionResults(parent, run.ID, run.ClaimToken, now); err != nil {
			return err
		}
		status = inspectiondomain.RunStatusCancelled
	}
	if status == inspectiondomain.RunStatusCompleted {
		switch {
		case applyErr != nil:
			status = inspectiondomain.RunStatusFailed
			message = "自动处置巡检结果失败: " + applyErr.Error()
		case autoResult.Failed > 0:
			status = inspectiondomain.RunStatusFailed
			message = fmt.Sprintf("自动处置失败 %d 项", autoResult.Failed)
		}
		s.logger.Info("account_inspection_auto_apply_finished", "run_id", run.ID, "provider", run.Provider, "applied", autoResult.Applied, "skipped", autoResult.Skipped, "failed", autoResult.Failed)
	}
	now = s.now()
	updated, err := s.runs.FinishInspectionRun(parent, run.ID, run.ClaimToken, status, message, now)
	if err != nil {
		return err
	}
	if updated {
		observability.ObserveAccountInspectionRun(string(run.Provider), string(status), now.Sub(started))
		s.logger.Info("account_inspection_finished", "run_id", run.ID, "provider", run.Provider, "status", status, "duration", now.Sub(started))
	}
	return nil
}

func (s *Service) watchRun(ctx context.Context, run inspectiondomain.Run, cancel context.CancelFunc, cancelled, leaseLost *atomic.Bool, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(s.watchPoll)
	defer ticker.Stop()
	lastRenewed := s.now()
	leaseUntil := lastRenewed.Add(s.lease)
	if run.LeaseUntil != nil {
		leaseUntil = *run.LeaseUntil
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		requested, err := s.runs.InspectionCancellationRequested(ctx, run.ID, run.ClaimToken)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			leaseLost.Store(true)
			cancel()
			return
		}
		if requested {
			cancelled.Store(true)
			cancel()
			return
		}
		now := s.now()
		if now.Sub(lastRenewed) < s.heartbeat {
			continue
		}
		renewed, renewErr := s.runs.RenewInspectionRun(ctx, run.ID, run.ClaimToken, now, now.Add(s.lease))
		if renewErr != nil {
			if ctx.Err() != nil {
				return
			}
			if now.Before(leaseUntil) {
				continue
			}
			leaseLost.Store(true)
			cancel()
			return
		}
		if !renewed {
			leaseLost.Store(true)
			cancel()
			return
		}
		lastRenewed = now
		leaseUntil = now.Add(s.lease)
	}
}

func (s *Service) inspectTarget(ctx context.Context, run inspectiondomain.Run, target inspectiondomain.Result) inspectiondomain.Result {
	result := target
	started := s.now()
	result.UpdatedAt = started
	credential, err := s.accounts.Get(ctx, target.AccountID)
	if err != nil {
		failure := gateway.ClassifyTransportFailure(err, target.AccountID, target.AccountName)
		return classifiedResult(result, failure, 0, 0, s.now().Sub(started))
	}
	adapter, ok := s.providers.Responses(run.Provider)
	if !ok {
		failure := gateway.ClassifyTransportFailure(errors.New("response adapter unavailable"), target.AccountID, target.AccountName)
		return classifiedResult(result, failure, 0, 0, s.now().Sub(started))
	}
	method, path, operation, body := inspectionRequest(run.UpstreamModel, run.ModelRouteID, s.models, ctx)
	var lastFailure *gateway.UpstreamFailure
	for attempt := 1; attempt <= 2; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, s.probeLimit)
		response, callErr := adapter.ForwardResponse(attemptCtx, provider.ResponseResourceRequest{
			Credential: credential, Method: method, Path: path, Model: run.UpstreamModel, Body: body,
			Streaming: false, NormalizeBody: true, Operation: operation,
		})
		if callErr != nil {
			cancel()
			lastFailure = gateway.ClassifyTransportFailure(callErr, credential.ID, credential.Name)
			if attempt < 2 && waitProbeRetry(ctx) {
				continue
			}
			return classifiedResult(result, lastFailure, 0, attempt, s.now().Sub(started))
		}
		status, responseBody, readErr := readProbeResponse(response)
		cancel()
		if readErr != nil {
			lastFailure = gateway.ClassifyTransportFailure(readErr, credential.ID, credential.Name)
			if attempt < 2 && waitProbeRetry(ctx) {
				continue
			}
			return classifiedResult(result, lastFailure, 0, attempt, s.now().Sub(started))
		}
		if status >= http.StatusOK && status < http.StatusMultipleChoices {
			result.Classification = inspectiondomain.ClassificationHealthy
			result.SuggestedAction = inspectiondomain.ActionClearHealth
			result.Confidence = inspectiondomain.ConfidenceHigh
			result.HTTPStatus = status
			result.Attempts = attempt
			result.Latency = s.now().Sub(started)
			result.UpdatedAt = s.now()
			return result
		}
		lastFailure = gateway.ClassifyHTTPFailure(status, responseBody, credential.ID, credential.Name)
		if attempt < 2 && (status == http.StatusTooManyRequests || status >= http.StatusInternalServerError) && waitProbeRetry(ctx) {
			continue
		}
		classified := classifiedResult(result, lastFailure, status, attempt, s.now().Sub(started))
		if status == http.StatusUnauthorized && credential.AuthType == account.AuthTypeOAuth && credential.EncryptedRefreshToken != "" {
			classified.Classification = inspectiondomain.ClassificationProbeError
			classified.SuggestedAction = inspectiondomain.ActionReview
			classified.Confidence = inspectiondomain.ConfidenceMedium
		}
		return classified
	}
	return classifiedResult(result, lastFailure, 0, 2, s.now().Sub(started))
}

func inspectionRequest(model string, routeID uint64, models repository.ModelRepository, ctx context.Context) (string, string, string, []byte) {
	capability := modeldomain.CapabilityResponses
	if route, err := models.Get(ctx, routeID); err == nil {
		capability = route.Capability
	}
	if capability == modeldomain.CapabilityChat {
		body, _ := json.Marshal(map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "ping"}}, "stream": false, "max_tokens": 8})
		return http.MethodPost, "/chat/completions", "chat", body
	}
	body, _ := json.Marshal(map[string]any{"model": model, "input": "ping", "stream": false, "max_output_tokens": 8})
	return http.MethodPost, "/responses", "responses", body
}

func readProbeResponse(response *provider.Response) (int, []byte, error) {
	if response == nil {
		return 0, nil, errors.New("provider returned an empty response")
	}
	if response.Body == nil {
		return response.StatusCode, nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxProbeResponseBody+1))
	_ = response.Body.Close()
	if err != nil {
		return response.StatusCode, nil, err
	}
	if len(body) > maxProbeResponseBody {
		body = body[:maxProbeResponseBody]
	}
	return response.StatusCode, body, nil
}

func waitProbeRetry(ctx context.Context) bool {
	timer := time.NewTimer(350 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func classifiedResult(result inspectiondomain.Result, failure *gateway.UpstreamFailure, status, attempts int, latency time.Duration) inspectiondomain.Result {
	result.Attempts = attempts
	result.Latency = latency
	result.HTTPStatus = status
	result.UpdatedAt = time.Now().UTC()
	if failure == nil {
		result.Classification = inspectiondomain.ClassificationProbeError
		result.SuggestedAction = inspectiondomain.ActionKeep
		result.Confidence = inspectiondomain.ConfidenceLow
		result.ErrorCode = "probe_failed"
		result.ErrorMessage = "probe failed without a classified response"
		return result
	}
	decision := gateway.DecideFailure(failure)
	result.FailureScope = string(decision.Scope)
	result.FailureAction = string(decision.Action)
	result.ErrorCode = failure.AuditCode()
	result.ErrorMessage = failure.PublicMessage
	result.QuotaExhausted = failure.QuotaExhausted
	result.FreeQuotaExhausted = failure.FreeQuotaExhausted
	result.ModelQuotaExhausted = failure.ModelQuotaExhausted
	result.CredentialRejected = failure.CredentialRejected
	result.PermanentAccountDenial = failure.PermanentAccountDenial
	switch {
	case failure.QuotaExhausted:
		result.Classification = inspectiondomain.ClassificationQuotaExhausted
		result.SuggestedAction = inspectiondomain.ActionUpdateQuota
		result.Confidence = inspectiondomain.ConfidenceHigh
	case failure.CredentialRejected || failure.HTTPStatus == http.StatusUnauthorized:
		result.Classification = inspectiondomain.ClassificationReauth
		result.SuggestedAction = inspectiondomain.ActionRequireReauth
		result.Confidence = inspectiondomain.ConfidenceHigh
	case failure.PermanentAccountDenial:
		result.Classification = inspectiondomain.ClassificationPermissionDenied
		result.SuggestedAction = inspectiondomain.ActionReview
		result.Confidence = inspectiondomain.ConfidenceHigh
	case failure.ModelUnavailable:
		result.Classification = inspectiondomain.ClassificationModelUnavailable
		result.SuggestedAction = inspectiondomain.ActionKeep
		result.Confidence = inspectiondomain.ConfidenceHigh
	default:
		result.Classification = inspectiondomain.ClassificationProbeError
		result.SuggestedAction = inspectiondomain.ActionKeep
		if failure.HTTPStatus == http.StatusTooManyRequests || failure.HTTPStatus == http.StatusForbidden {
			result.Confidence = inspectiondomain.ConfidenceMedium
		} else {
			result.Confidence = inspectiondomain.ConfidenceLow
		}
	}
	return result
}

func (s *Service) applyRunResults(ctx context.Context, run inspectiondomain.Run) (ApplyResult, error) {
	var all []inspectiondomain.Result
	for offset := 0; ; offset += 500 {
		values, total, listErr := s.runs.ListInspectionResults(ctx, run.ID, offset, 500)
		if listErr != nil {
			return ApplyResult{}, listErr
		}
		all = append(all, values...)
		if int64(len(all)) >= total {
			break
		}
	}
	result := ApplyResult{}
	for _, value := range all {
		applyToken, tokenErr := newInspectionToken()
		if tokenErr != nil {
			return result, tokenErr
		}
		now := s.now()
		claimed, claimErr := s.runs.TryClaimInspectionResultApplication(ctx, run.ID, value.AccountID, run.ClaimToken, applyToken, now, now.Add(s.lease))
		if claimErr != nil {
			return result, claimErr
		}
		if !claimed {
			result.Skipped++
			continue
		}
		finish := func(status inspectiondomain.ApplyStatus, message string) error {
			updated, finishErr := s.runs.FinishInspectionResultApplication(context.WithoutCancel(ctx), run.ID, value.AccountID, run.ClaimToken, applyToken, status, string(value.SuggestedAction), message, s.now())
			if finishErr != nil {
				return finishErr
			}
			if !updated {
				return repository.ErrConflict
			}
			return nil
		}
		if value.Confidence != inspectiondomain.ConfidenceHigh {
			if err := finish(inspectiondomain.ApplyStatusSkipped, "confidence_not_high"); err != nil {
				return result, err
			}
			result.Skipped++
			continue
		}
		if err := s.applyInferenceEvidence(ctx, value); err != nil {
			if errors.Is(err, ErrStaleEvidence) {
				if finishErr := finish(inspectiondomain.ApplyStatusSkipped, "stale_evidence"); finishErr != nil {
					return result, finishErr
				}
				result.Skipped++
				continue
			}
			if finishErr := finish(inspectiondomain.ApplyStatusFailed, err.Error()); finishErr != nil {
				return result, finishErr
			}
			result.Failed++
			continue
		}
		if value.SuggestedAction == inspectiondomain.ActionKeep || value.SuggestedAction == inspectiondomain.ActionReview {
			if err := finish(inspectiondomain.ApplyStatusSkipped, "action_not_automatic"); err != nil {
				return result, err
			}
			result.Skipped++
			continue
		}
		if err := s.applyResult(ctx, value); err != nil {
			if errors.Is(err, ErrStaleEvidence) {
				if finishErr := finish(inspectiondomain.ApplyStatusSkipped, "stale_evidence"); finishErr != nil {
					return result, finishErr
				}
				result.Skipped++
				continue
			}
			if finishErr := finish(inspectiondomain.ApplyStatusFailed, err.Error()); finishErr != nil {
				return result, finishErr
			}
			result.Failed++
			s.logger.Warn("account_inspection_apply_failed", "run_id", run.ID, "account_id", value.AccountID, "action", value.SuggestedAction, "error", err)
			continue
		}
		if err := finish(inspectiondomain.ApplyStatusApplied, ""); err != nil {
			return result, err
		}
		result.Applied++
	}
	return result, nil
}

func (s *Service) applyInferenceEvidence(ctx context.Context, value inspectiondomain.Result) error {
	if s.selector == nil {
		return nil
	}
	credential, err := s.accounts.Get(ctx, value.AccountID)
	if err != nil {
		return err
	}
	if !value.AccountUpdatedAt.IsZero() && credential.UpdatedAt.After(value.AccountUpdatedAt) {
		return ErrStaleEvidence
	}
	switch value.Classification {
	case inspectiondomain.ClassificationHealthy:
		return s.selector.ApplyInferenceHealth(ctx, value.AccountID, value.Model, account.InferenceHealthHealthy, value.HTTPStatus, value.ErrorCode)
	case inspectiondomain.ClassificationPermissionDenied:
		return s.selector.ApplyInferenceHealth(ctx, value.AccountID, value.Model, account.InferenceHealthPermissionDenied, value.HTTPStatus, value.ErrorCode)
	case inspectiondomain.ClassificationModelUnavailable:
		return s.selector.ApplyInferenceHealth(ctx, value.AccountID, value.Model, account.InferenceHealthModelUnavailable, value.HTTPStatus, value.ErrorCode)
	case inspectiondomain.ClassificationReauth:
		return s.selector.ApplyInferenceHealth(ctx, value.AccountID, value.Model, account.InferenceHealthReauth, value.HTTPStatus, value.ErrorCode)
	}
	return nil
}

func (s *Service) applyResult(ctx context.Context, value inspectiondomain.Result) error {
	credential, err := s.accounts.Get(ctx, value.AccountID)
	if err != nil {
		return err
	}
	if !value.AccountUpdatedAt.IsZero() && credential.UpdatedAt.After(value.AccountUpdatedAt) {
		return ErrStaleEvidence
	}
	switch value.SuggestedAction {
	case inspectiondomain.ActionClearHealth:
		if s.credentials == nil || s.selector == nil {
			return errors.New("inspection evidence target unavailable")
		}
		credential, err = s.credentials.MarkInspectionHealthy(ctx, value.AccountID)
		if err != nil {
			return err
		}
		return s.selector.ApplyInspectionHealthy(ctx, credential, value.Model)
	case inspectiondomain.ActionRequireReauth:
		if s.credentials == nil || s.selector == nil {
			return errors.New("inspection evidence target unavailable")
		}
		if err := s.credentials.MarkReauthRequired(ctx, value.AccountID, fmt.Sprintf("%s inspection confirmed credential rejection", value.Provider)); err != nil {
			return err
		}
		s.selector.MarkQuotaStateChanged(credential.Provider)
		return nil
	case inspectiondomain.ActionUpdateQuota:
		if s.selector == nil {
			return errors.New("inspection quota target unavailable")
		}
		if value.ModelQuotaExhausted {
			return s.selector.ApplyModelQuotaExhausted(ctx, credential, value.Model, 24*time.Hour)
		}
		if kind, _ := s.providers.QuotaKind(credential.Provider); kind == provider.QuotaBilling {
			var billing *account.Billing
			if current, billingErr := s.accounts.GetBilling(ctx, credential.ID); billingErr == nil {
				billing = &current
			}
			return s.selector.ApplyPaidQuotaExhausted(ctx, credential, billing)
		}
		return s.selector.ApplyFreeQuotaExhausted(ctx, credential, 0, 0)
	case inspectiondomain.ActionKeep, inspectiondomain.ActionReview:
		return nil
	default:
		return ErrInvalidInput
	}
}

func newInspectionToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
