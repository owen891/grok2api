package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func grpcWebFrame(payload []byte) []byte {
	frame := make([]byte, 5+len(payload))
	frame[0] = 0
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

func nsfwPayload(enabled bool) []byte {
	value := byte(0)
	if enabled {
		value = 1
	}
	name := []byte("always_show_nsfw_content")
	inner := append([]byte{0x0a, byte(len(name))}, name...)
	proto := []byte{0x0a, 0x02, 0x10, value, 0x12, byte(len(inner))}
	proto = append(proto, inner...)
	return grpcWebFrame(proto)
}

func acceptTosPayload() []byte { return grpcWebFrame([]byte{0x10, 0x01}) }

func randomBirthDate() string {
	now := time.Now().UTC()
	buf := make([]byte, 2)
	_, _ = rand.Read(buf)
	year := now.Year() - 20 - int(binary.BigEndian.Uint16(buf)%29)
	return fmt.Sprintf("%04d-01-01T00:00:00.000Z", year)
}

func (a *Adapter) SetNSFW(ctx context.Context, credential account.Credential, enabled bool) error {
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return err
	}
	lease, err := a.egress.Acquire(ctx, domainegress.ScopeWeb, strconv.FormatUint(credential.ID, 10))
	if err != nil {
		return err
	}
	defer lease.Release()
	if enabled {
		if err := a.nsfwGRPC(ctx, lease, "https://accounts.x.ai/auth_mgmt.AuthManagement/SetTosAcceptedVersion", token, acceptTosPayload(), "https://accounts.x.ai", "https://accounts.x.ai/accept-tos"); err != nil {
			return err
		}
		birthURL := stringsTrimRight(a.config().BaseURL) + "/rest/auth/set-birth-date"
		body, _ := json.Marshal(map[string]string{"birthDate": randomBirthDate()})
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, birthURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		request.Header = buildHeaders(token, lease, "application/json")
		applyAppHeaders(request.Header, a.config().BaseURL, a.config().BaseURL+"/?_s=data")
		response, err := lease.Do(request)
		if err != nil {
			return err
		}
		birthBody, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		if readErr != nil {
			return readErr
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			if !(response.StatusCode == http.StatusTooManyRequests && bytes.Contains(bytes.ToLower(birthBody), []byte("birth-date-change-limit-reached"))) {
				return fmt.Errorf("set birth date returned %d", response.StatusCode)
			}
		}
	}
	return a.nsfwGRPC(ctx, lease, stringsTrimRight(a.config().BaseURL)+"/auth_mgmt.AuthManagement/UpdateUserFeatureControls", token, nsfwPayload(enabled), a.config().BaseURL, a.config().BaseURL+"/?_s=data")
}

func stringsTrimRight(value string) string {
	for len(value) > 0 && value[len(value)-1] == '/' {
		value = value[:len(value)-1]
	}
	return value
}

func (a *Adapter) nsfwGRPC(ctx context.Context, lease *infraegress.Lease, endpoint, token string, body []byte, origin, referer string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header = buildHeaders(token, lease, "application/grpc-web+proto")
	applyAppHeaders(request.Header, origin, referer)
	request.Header.Set("x-grpc-web", "1")
	request.Header.Set("x-user-agent", "connect-es/2.1.1")
	response, err := lease.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("gRPC Web returned %d", response.StatusCode)
	}
	if _, err := firstGRPCWebMessage(data); err != nil {
		return err
	}
	return nil
}

var _ provider.NSFWAdapter = (*Adapter)(nil)
