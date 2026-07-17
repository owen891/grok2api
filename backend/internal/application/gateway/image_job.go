package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

const (
	imageJobTimeout        = 30 * time.Minute
	maxImageJobOutputBytes = int64(128 << 20)
)

type AsyncImageInput struct {
	RequestID      string
	ClientKey      clientkey.Key
	PublicModel    string
	Prompt         string
	Count          int
	Size           string
	AspectRatio    string
	Resolution     string
	ResponseFormat string
}

type asyncImagePayload struct {
	PublicModel    string `json:"model"`
	Count          int    `json:"n"`
	Size           string `json:"size,omitempty"`
	AspectRatio    string `json:"aspect_ratio,omitempty"`
	Resolution     string `json:"resolution,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
}

func (s *Service) CreateImageJob(ctx context.Context, input AsyncImageInput) (media.Job, error) {
	if s.mediaJobs == nil || s.mediaQueue == nil {
		return media.Job{}, fmt.Errorf("图片任务服务未配置")
	}
	routes, err := s.models.GetByPublicIDCandidates(ctx, input.PublicModel)
	if err != nil {
		return media.Job{}, ErrModelNotFound
	}
	route, err := s.selectMediaRoute(routes, input.ClientKey, model.CapabilityImage, func(providerValue account.Provider) bool {
		_, ok := s.providers.Images(providerValue)
		return ok
	})
	if err != nil {
		return media.Job{}, err
	}
	quotaMode := s.providers.QuotaMode(route.Provider, route.UpstreamModel)
	lease, err := s.selector.Acquire(ctx, route.Provider, route.UpstreamModel, quotaMode, "", nil, false)
	if err != nil {
		return media.Job{}, fmt.Errorf("%w: %w", ErrNoAvailableAccount, err)
	}
	credential := lease.Credential
	lease.Release()
	token, err := security.NewOpaqueToken(18)
	if err != nil {
		return media.Job{}, err
	}
	payload, err := json.Marshal(asyncImagePayload{
		PublicModel: input.PublicModel, Count: input.Count, Size: input.Size,
		AspectRatio: input.AspectRatio, Resolution: input.Resolution, ResponseFormat: input.ResponseFormat,
	})
	if err != nil {
		return media.Job{}, err
	}
	spec := strings.TrimSpace(input.AspectRatio)
	if spec == "" {
		spec = strings.TrimSpace(input.Size)
	}
	if spec == "" {
		spec = "auto"
	}
	quality := strings.TrimSpace(input.Resolution)
	if quality == "" {
		quality = "1k"
	}
	now := time.Now().UTC()
	job := media.Job{
		ID: "image_" + token, Kind: media.JobKindImage, RequestID: input.RequestID,
		ClientKeyID: input.ClientKey.ID, ClientKeyName: input.ClientKey.Name,
		AccountID: credential.ID, AccountName: credential.Name,
		Provider: string(route.Provider), Model: model.ExternalPublicID(route.Provider, route.PublicID),
		ModelRouteID: route.ID, UpstreamModel: model.DisplayUpstreamModel(route.Provider, route.UpstreamModel),
		Prompt: input.Prompt, Seconds: 1, Size: spec, Quality: quality,
		Status: media.StatusQueued, Progress: 0, InputJSON: string(payload), CreatedAt: now, UpdatedAt: now,
	}
	if err := s.mediaJobs.CreateMediaJob(ctx, job); err != nil {
		return media.Job{}, err
	}
	if !s.enqueueVideoJob(job.ID) {
		s.logger.Warn("image_job_queue_full", "job_id", job.ID)
	}
	return job, nil
}

func (s *Service) GetImageJob(ctx context.Context, id string, key clientkey.Key) (media.Job, error) {
	if s.mediaJobs == nil {
		return media.Job{}, ErrResponseNotFound
	}
	job, err := s.mediaJobs.GetMediaJob(ctx, id, key.ID)
	if err != nil || job.Kind != media.JobKindImage {
		return media.Job{}, ErrResponseNotFound
	}
	return job, nil
}

func (s *Service) processImageJob(ctx context.Context, id string) {
	job, claimed, err := s.claimVideoJob(ctx, id)
	if err != nil {
		s.logger.Warn("image_job_claim_failed", "job_id", id, "error", err)
		return
	}
	if !claimed || job.Kind != media.JobKindImage {
		return
	}
	key, err := s.clientKeys.GetForJob(ctx, job.ClientKeyID)
	if err != nil || !key.IsAvailable(time.Now().UTC()) {
		s.failImageJob(ctx, job, "client_key_unavailable", errors.New("客户端 Key 已失效"))
		return
	}
	if !s.clientKeys.CanUseModel(key, job.ModelRouteID) {
		s.failImageJob(ctx, job, "model_not_allowed", errors.New("客户端 Key 未获准使用该模型"))
		return
	}
	var payload asyncImagePayload
	if err := json.Unmarshal([]byte(job.InputJSON), &payload); err != nil {
		s.failImageJob(ctx, job, "invalid_job_input", err)
		return
	}
	job.Progress = max(job.Progress, 1)
	job.UpdatedAt = time.Now().UTC()
	if err := s.mediaJobs.UpdateMediaJob(ctx, job); err != nil {
		s.logger.Warn("image_job_progress_write_failed", "job_id", id, "error", err)
	}
	workCtx, cancel := context.WithTimeout(ctx, imageJobTimeout)
	defer cancel()
	result, err := s.GenerateImage(workCtx, ImageGenerationInput{
		RequestID: job.RequestID, ClientKey: key, PublicModel: payload.PublicModel, Prompt: job.Prompt,
		Count: payload.Count, Size: payload.Size, AspectRatio: payload.AspectRatio,
		Resolution: payload.Resolution, ResponseFormat: payload.ResponseFormat,
	})
	if err != nil {
		if ctx.Err() != nil {
			s.deferVideoJob(ctx, job)
			return
		}
		code, message := "generation_failed", err.Error()
		var failure *UpstreamFailure
		if errors.As(err, &failure) {
			code, message = failure.Code, failure.PublicMessage
		}
		s.failImageJob(ctx, job, code, errors.New(message))
		return
	}
	body, readErr := io.ReadAll(io.LimitReader(result.Body, maxImageJobOutputBytes+1))
	errorCode := ""
	if readErr != nil {
		errorCode = "response_read_failed"
	} else if int64(len(body)) > maxImageJobOutputBytes {
		readErr = errors.New("图片响应超过 128 MiB 持久化上限")
		errorCode = "response_too_large"
	} else if result.StatusCode < http.StatusOK || result.StatusCode >= http.StatusMultipleChoices {
		readErr = fmt.Errorf("图片上游返回状态 %d", result.StatusCode)
		errorCode = "upstream_error"
	} else if !json.Valid(body) {
		readErr = errors.New("图片上游返回了无效 JSON")
		errorCode = "invalid_response"
	}
	result.Finalize(Usage{}, "", errorCode)
	_ = result.Body.Close()
	if readErr != nil {
		s.failImageJob(ctx, job, errorCode, readErr)
		return
	}
	now := time.Now().UTC()
	job.Status, job.Progress = media.StatusCompleted, 100
	job.OutputJSON, job.ContentType = string(body), "application/json"
	job.LeaseUntil, job.UpdatedAt, job.CompletedAt, job.UsageRecordedAt = nil, now, &now, &now
	if err := s.persistVideoJobWithRetry(ctx, job); err != nil {
		s.logger.Error("image_job_terminal_write_failed", "job_id", job.ID, "error", err)
	}
}

func (s *Service) failImageJob(ctx context.Context, job media.Job, code string, err error) {
	now := time.Now().UTC()
	job.Status, job.ErrorCode, job.ErrorMessage = media.StatusFailed, code, err.Error()
	if len(job.ErrorMessage) > 512 {
		job.ErrorMessage = job.ErrorMessage[:512]
	}
	job.LeaseUntil, job.UpdatedAt, job.CompletedAt, job.UsageRecordedAt = nil, now, &now, &now
	if updateErr := s.persistVideoJobWithRetry(ctx, job); updateErr != nil {
		s.logger.Error("image_job_terminal_write_failed", "job_id", job.ID, "error", updateErr)
	}
}
