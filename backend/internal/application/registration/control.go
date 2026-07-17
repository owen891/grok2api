package registration

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxRegistrationLogLines = 500
	maxRegistrationLogBytes = 5 << 20
)

var (
	ErrRunning       = errors.New("注册任务正在运行")
	ErrNotConfigured = errors.New("注册 worker 未配置")
	ErrInvalidInput  = errors.New("注册任务参数无效")
	ErrPreflight     = errors.New("注册 worker 预检失败")
)

type Controller struct {
	logger *slog.Logger
	config Config

	mu      sync.Mutex
	command *exec.Cmd
	done    chan struct{}
}

type StartInput struct {
	Count       int    `json:"count"`
	Extra       int    `json:"extra"`
	Threads     int    `json:"threads"`
	Fast        bool   `json:"fast"`
	AccountType string `json:"accountType"`
	AutoNSFW    bool   `json:"autoNSFW"`
}

type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Progress struct {
	Mode          string   `json:"mode"`
	Done          int      `json:"done"`
	Total         *int     `json:"total"`
	Percent       *float64 `json:"percent"`
	Indeterminate bool     `json:"indeterminate"`
	AccountCount  int      `json:"accountCount"`
	Attempted     int      `json:"attempted"`
	Succeeded     int      `json:"succeeded"`
	Failed        int      `json:"failed"`
	Resumable     int      `json:"resumable"`
}

type Status struct {
	Configured bool       `json:"configured"`
	Running    bool       `json:"running"`
	PID        int        `json:"pid,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	ExitCode   *int       `json:"exitCode,omitempty"`
	LastError  *Failure   `json:"lastError,omitempty"`
	Progress   Progress   `json:"progress"`
}

type LogEntry struct {
	ID   uint64 `json:"id"`
	Text string `json:"text"`
}

type LogResult struct {
	Items     []LogEntry `json:"items"`
	NextLogID uint64     `json:"nextLogId"`
}

type PreflightCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type PreflightResult struct {
	OK     bool             `json:"ok"`
	Checks []PreflightCheck `json:"checks"`
	Config WorkerSettings   `json:"config"`
}

type PreflightError struct {
	Checks []PreflightCheck
}

func (e *PreflightError) Error() string {
	failed := make([]string, 0, len(e.Checks))
	for _, check := range e.Checks {
		if !check.OK {
			failed = append(failed, check.Name+": "+check.Detail)
		}
	}
	if len(failed) == 0 {
		return ErrPreflight.Error()
	}
	return ErrPreflight.Error() + " (" + strings.Join(failed, "; ") + ")"
}

func (e *PreflightError) Unwrap() error { return ErrPreflight }

type EmailSourceSettings struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Enabled          bool   `json:"enabled"`
	APIBase          string `json:"apiBase"`
	APIKey           string `json:"apiKey"`
	JWT              string `json:"jwt"`
	Domain           string `json:"domain"`
	Prefix           string `json:"prefix"`
	APIKeyConfigured bool   `json:"apiKeyConfigured"`
	JWTConfigured    bool   `json:"jwtConfigured"`
}

type storedEmailSource struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
	APIBase string `json:"api_base,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
	JWT     string `json:"jwt,omitempty"`
	Domain  string `json:"domain,omitempty"`
	Prefix  string `json:"prefix,omitempty"`
}

type WorkerSettings struct {
	Engine                   string                `json:"engine"`
	EmailSources             []EmailSourceSettings `json:"emailSources"`
	EmailProvider            string                `json:"emailProvider"`
	EmailProviderFallbacks   []string              `json:"emailProviderFallbacks"`
	TempmailLolAPIBase       string                `json:"tempmailLolApiBase"`
	TempmailLolDomain        string                `json:"tempmailLolDomain"`
	TempmailLolPrefix        string                `json:"tempmailLolPrefix"`
	Proxy                    string                `json:"proxy"`
	CPABaseURL               string                `json:"cpaBaseURL"`
	CPAProxy                 string                `json:"cpaProxy"`
	CPAHeadless              bool                  `json:"cpaHeadless"`
	CPAProbeChat             bool                  `json:"cpaProbeChat"`
	CPACloseBrowserAfterAuth bool                  `json:"cpaCloseBrowserAfterAuth"`
	CaptchaSolver            string                `json:"captchaSolver"`
	CaptchaEndpoint          string                `json:"captchaEndpoint"`
	YydsAPIKey               string                `json:"yydsApiKey"`
	YydsJWT                  string                `json:"yydsJwt"`
	YesCaptchaAPIKey         string                `json:"yescaptchaApiKey"`
	YydsAPIKeyConfigured     bool                  `json:"yydsApiKeyConfigured"`
	YydsJWTConfigured        bool                  `json:"yydsJwtConfigured"`
	YesCaptchaKeyConfigured  bool                  `json:"yescaptchaApiKeyConfigured"`
}

type WorkerSettingsPatch struct {
	Engine                   *string                `json:"engine"`
	EmailSources             *[]EmailSourceSettings `json:"emailSources"`
	EmailProvider            *string                `json:"emailProvider"`
	EmailProviderFallbacks   *[]string              `json:"emailProviderFallbacks"`
	TempmailLolAPIBase       *string                `json:"tempmailLolApiBase"`
	TempmailLolDomain        *string                `json:"tempmailLolDomain"`
	TempmailLolPrefix        *string                `json:"tempmailLolPrefix"`
	Proxy                    *string                `json:"proxy"`
	CPABaseURL               *string                `json:"cpaBaseURL"`
	CPAProxy                 *string                `json:"cpaProxy"`
	CPAHeadless              *bool                  `json:"cpaHeadless"`
	CPAProbeChat             *bool                  `json:"cpaProbeChat"`
	CPACloseBrowserAfterAuth *bool                  `json:"cpaCloseBrowserAfterAuth"`
	CaptchaSolver            *string                `json:"captchaSolver"`
	CaptchaEndpoint          *string                `json:"captchaEndpoint"`
	YydsAPIKey               *string                `json:"yydsApiKey"`
	YydsJWT                  *string                `json:"yydsJwt"`
	YesCaptchaAPIKey         *string                `json:"yescaptchaApiKey"`
}

type persistedState struct {
	Engine              string     `json:"engine,omitempty"`
	Running             bool       `json:"running"`
	PID                 int        `json:"pid,omitempty"`
	StartedAt           *time.Time `json:"startedAt,omitempty"`
	FinishedAt          *time.Time `json:"finishedAt,omitempty"`
	ExitCode            *int       `json:"exitCode,omitempty"`
	LastError           *Failure   `json:"lastError,omitempty"`
	StopRequested       bool       `json:"stopRequested,omitempty"`
	ProgressMode        string     `json:"progressMode"`
	InitialAccountCount int        `json:"initialAccountCount"`
	TargetCount         *int       `json:"targetCount,omitempty"`
}

func NewController(logger *slog.Logger, config Config) *Controller {
	if logger == nil {
		logger = slog.Default()
	}
	return &Controller{logger: logger, config: config}
}

func (c *Controller) Status() (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.reconcileStateLocked()
	if err != nil {
		return Status{}, err
	}
	return c.statusLocked(state), nil
}

func (c *Controller) Start(ctx context.Context, input StartInput) (Status, error) {
	if input.Count < 0 || input.Count > 10000 || input.Extra < 0 || input.Extra > 10000 || input.Threads < 1 || input.Threads > 10 {
		return Status{}, ErrInvalidInput
	}
	accountType := strings.ToLower(strings.TrimSpace(input.AccountType))
	if accountType == "" {
		accountType = "build"
	}
	if accountType != "build" && accountType != "web" {
		return Status{}, ErrInvalidInput
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.reconcileStateLocked()
	if err != nil {
		return Status{}, err
	}
	if state.Running {
		return Status{}, ErrRunning
	}
	if err := c.ensureWorkerConfigLocked(); err != nil {
		return Status{}, err
	}
	preflight := c.preflightLocked(ctx)
	if !preflight.OK {
		return Status{}, &PreflightError{Checks: preflight.Checks}
	}
	effectiveProxy, proxyOK, _ := resolveRegistrationProxy(preflight.Config.Proxy)
	if !proxyOK {
		return Status{}, &PreflightError{Checks: []PreflightCheck{{Name: "proxy", Detail: "resolved proxy is not reachable"}}}
	}
	if err := c.acquireLockLocked(); err != nil {
		return Status{}, err
	}
	if err := c.resetLogsLocked(); err != nil {
		c.releaseLockLocked()
		return Status{}, err
	}

	mode := "count"
	value := input.Count
	if input.Extra > 0 {
		mode = "extra"
		value = input.Extra
	} else if input.Count == 0 {
		value = 1
	}
	target := &value
	if err := os.Remove(c.protocolStatePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		c.releaseLockLocked()
		return Status{}, fmt.Errorf("清理协议注册状态: %w", err)
	}

	protocolScript := filepath.Join(c.config.WorkDir, "protocol_register_cli.py")
	if _, err := os.Stat(protocolScript); err != nil {
		c.releaseLockLocked()
		return Status{}, fmt.Errorf("协议注册脚本不存在: %w", err)
	}
	arguments := protocolWorkerArguments(c.config.Command, protocolScript,
		"--config", c.config.ConfigPath,
		"--state-dir", c.dataPath(),
		"--log-file", c.logPath(),
		"--count", strconv.Itoa(input.Count),
		"--threads", strconv.Itoa(input.Threads),
		"--account-type", accountType,
	)
	if effectiveProxy != "" {
		arguments = append(arguments, "--proxy", effectiveProxy)
	}
	if input.Extra > 0 {
		arguments = append(arguments, "--extra", strconv.Itoa(input.Extra))
	}
	if input.Fast {
		arguments = append(arguments, "--fast")
	}
	if accountType == "web" && input.AutoNSFW {
		arguments = append(arguments, "--auto-nsfw")
	}
	command := exec.Command(c.config.Command[0], arguments...)
	c.appendLogLocked(fmt.Sprintf("[website] 启动协议注册任务: 类型=%s 数量=%d 追加=%d 线程=%d", accountType, input.Count, input.Extra, input.Threads))
	command.Dir = c.config.WorkDir
	command.Env = c.workerEnvironment()
	prepareProcess(command)
	reader, writer, err := os.Pipe()
	if err != nil {
		c.releaseLockLocked()
		return Status{}, fmt.Errorf("创建注册任务日志管道: %w", err)
	}
	command.Stdout = writer
	command.Stderr = writer
	if err := command.Start(); err != nil {
		reader.Close()
		writer.Close()
		c.releaseLockLocked()
		return Status{}, fmt.Errorf("启动注册 worker: %w", err)
	}
	writer.Close()
	now := time.Now().UTC()
	state = persistedState{
		Engine: "protocol", Running: true, PID: command.Process.Pid, StartedAt: &now,
		ProgressMode: mode, TargetCount: target,
	}
	if err := c.writeStateLocked(state); err != nil {
		_ = stopProcessTree(context.Background(), command.Process.Pid)
		reader.Close()
		c.releaseLockLocked()
		return Status{}, err
	}
	c.command = command
	c.done = make(chan struct{})
	go c.monitor(command, reader, c.done)
	return c.statusLocked(state), nil
}

func (c *Controller) Stop(ctx context.Context) (Status, error) {
	c.mu.Lock()
	state, err := c.reconcileStateLocked()
	if err != nil {
		c.mu.Unlock()
		return Status{}, err
	}
	if !state.Running || state.PID == 0 {
		status := c.statusLocked(state)
		c.mu.Unlock()
		return status, nil
	}
	state.StopRequested = true
	_ = c.writeStateLocked(state)
	c.appendLogLocked("[website] 正在停止注册任务")
	done := c.done
	pid := state.PID
	c.mu.Unlock()

	stopErr := stopProcessTree(ctx, pid)
	if stopErr != nil {
		c.logger.Warn("registration_process_stop_failed", "pid", pid, "error", stopErr)
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err = c.reconcileStateLocked()
	if err != nil {
		return Status{}, err
	}
	if stopErr != nil && isRegistrationProcess(pid) {
		return c.statusLocked(state), fmt.Errorf("停止注册 worker: %w", stopErr)
	}
	if state.Running {
		now := time.Now().UTC()
		code := -1
		state.Running = false
		state.FinishedAt = &now
		state.ExitCode = &code
		state.LastError = &Failure{Code: "registrationStopped", Message: "注册任务已停止"}
		_ = c.writeStateLocked(state)
		c.releaseLockLocked()
	}
	return c.statusLocked(state), nil
}

func (c *Controller) Close(ctx context.Context) error {
	_, err := c.Stop(ctx)
	return err
}

func (c *Controller) Logs(limit int) (LogResult, error) {
	if limit < 1 || limit > maxRegistrationLogLines {
		limit = maxRegistrationLogLines
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, err := readLogEntries(c.logPath(), limit)
	if err != nil {
		return LogResult{}, err
	}
	result := LogResult{Items: entries}
	if len(entries) > 0 {
		result.NextLogID = entries[0].ID
	}
	return result, nil
}

func (c *Controller) Settings() (WorkerSettings, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureWorkerConfigLocked(); err != nil {
		return WorkerSettings{}, err
	}
	value, err := readJSONMap(c.config.ConfigPath)
	if err != nil {
		return WorkerSettings{}, err
	}
	return settingsView(value), nil
}

func (c *Controller) UpdateSettings(patch WorkerSettingsPatch) (WorkerSettings, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.reconcileStateLocked()
	if err != nil {
		return WorkerSettings{}, err
	}
	if state.Running {
		return WorkerSettings{}, ErrRunning
	}
	if err := c.ensureWorkerConfigLocked(); err != nil {
		return WorkerSettings{}, err
	}
	value, err := readJSONMap(c.config.ConfigPath)
	if err != nil {
		return WorkerSettings{}, err
	}
	if err := applySettingsPatch(value, patch); err != nil {
		return WorkerSettings{}, err
	}
	c.forceSafeWorkerSettings(value)
	if err := writeJSONAtomic(c.config.ConfigPath, value, 0o600); err != nil {
		return WorkerSettings{}, err
	}
	return settingsView(value), nil
}

func (c *Controller) Preflight(ctx context.Context) PreflightResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.preflightLocked(ctx)
}

func (c *Controller) preflightLocked(ctx context.Context) PreflightResult {
	if !c.config.Enabled {
		return PreflightResult{
			OK:     false,
			Checks: []PreflightCheck{{Name: "enabled", OK: false, Detail: "registration worker is disabled"}},
		}
	}
	checks := make([]PreflightCheck, 0, 9)
	add := func(name string, ok bool, detail string) {
		checks = append(checks, PreflightCheck{Name: name, OK: ok, Detail: detail})
	}
	workInfo, workErr := os.Stat(c.config.WorkDir)
	add("workdir", workErr == nil && workInfo.IsDir(), c.config.WorkDir)
	commandPath, commandErr := resolveCommand(c.config.Command)
	add("worker", commandErr == nil, commandPath)
	configErr := c.ensureWorkerConfigLocked()
	add("config", configErr == nil, c.config.ConfigPath)
	dataErr := ensurePrivateDirectory(c.dataPath())
	add("registrationData", dataErr == nil, c.dataPath())
	spoolErr := ensurePrivateDirectory(filepath.Join(c.config.SpoolPath, "incoming"))
	add("spool", spoolErr == nil, c.config.SpoolPath)

	settings := WorkerSettings{}
	configValue := map[string]any{}
	if configErr == nil {
		configValue, _ = readJSONMap(c.config.ConfigPath)
		settings = settingsView(configValue)
	}
	add("engine", true, "protocol")
	enabledSources := make([]EmailSourceSettings, 0, len(settings.EmailSources))
	credentialsReady := true
	for _, source := range settings.EmailSources {
		if !source.Enabled {
			continue
		}
		enabledSources = append(enabledSources, source)
		if source.Type == "yyds" && !source.APIKeyConfigured && !source.JWTConfigured {
			credentialsReady = false
		}
	}
	providerNames := make([]string, 0, len(enabledSources))
	for _, source := range enabledSources {
		providerNames = append(providerNames, source.Type)
	}
	add("emailSources", len(enabledSources) > 0, strings.Join(providerNames, ", "))
	add("emailCredentials", len(enabledSources) > 0 && credentialsReady, "API key or YYDS JWT for every enabled source")
	// 协议路径不依赖浏览器 CPA 探测地址；仅校验 proxy 可选性。
	add("cpaBaseURL", true, "protocol mode skips browser CPA probe")
	_, proxyOK, proxyDetail := resolveRegistrationProxy(settings.Proxy)
	add("proxy", proxyOK, proxyDetail)
	add("cpaProxy", true, "protocol mode skips cpaProxy")
	solver := strings.ToLower(strings.TrimSpace(stringValue(configValue["captcha_solver"], "local")))
	if solver == "" {
		solver = "local"
	}
	yesKey := strings.TrimSpace(stringValue(configValue["yescaptcha_api_key"], ""))
	if yesKey == "" {
		yesKey = strings.TrimSpace(stringValue(configValue["yes_captcha_key"], ""))
	}
	if yesKey == "" {
		yesKey = strings.TrimSpace(stringValue(configValue["captcha_api_key"], ""))
	}
	// YYDS 的 AC- key 不能当 YesCaptcha
	if strings.HasPrefix(yesKey, "AC-") {
		yesKey = ""
	}
	if solver == "local" {
		ep := strings.TrimSpace(stringValue(configValue["captcha_endpoint"], ""))
		if ep == "" {
			ep = strings.TrimSpace(stringValue(configValue["local_captcha_endpoint"], ""))
		}
		add("captchaEndpoint", ep != "", ep)
		add("captchaSolver", true, "local-http")
	} else {
		add("yescaptcha", yesKey != "", "yescaptcha_api_key")
	}
	protocolScript := filepath.Join(c.config.WorkDir, "protocol_register_cli.py")
	_, protocolErr := os.Stat(protocolScript)
	add("protocolWorker", protocolErr == nil, protocolScript)

	dependencyOK := false
	dependencyDetail := "worker unavailable"
	if commandErr == nil && workErr == nil {
		probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		arguments := protocolWorkerArguments(c.config.Command, protocolScript,
			"--preflight",
			"--config", c.config.ConfigPath,
			"--state-dir", c.dataPath(),
			"--log-file", c.logPath(),
		)
		probe := exec.CommandContext(probeCtx, c.config.Command[0], arguments...)
		probe.Dir = c.config.WorkDir
		probe.Env = c.workerEnvironment()
		output, err := probe.CombinedOutput()
		dependencyOK = err == nil
		dependencyDetail = "ready"
		if err != nil {
			dependencyDetail = strings.TrimSpace(string(output))
			if dependencyDetail == "" {
				dependencyDetail = err.Error()
			}
		}
	}
	add("dependencies", dependencyOK, truncateText(dependencyDetail, 500))
	result := PreflightResult{OK: true, Checks: checks, Config: settings}
	for _, check := range checks {
		result.OK = result.OK && check.OK
	}
	return result
}

func (c *Controller) monitor(command *exec.Cmd, reader *os.File, done chan struct{}) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		c.mu.Lock()
		c.appendLogLocked(scanner.Text())
		c.mu.Unlock()
	}
	reader.Close()
	err := command.Wait()
	exitCode := command.ProcessState.ExitCode()
	c.mu.Lock()
	defer c.mu.Unlock()
	state, loadErr := c.readStateLocked()
	if loadErr != nil {
		c.logger.Error("registration_state_read_failed", "error", loadErr)
		close(done)
		return
	}
	if state.PID == command.Process.Pid {
		now := time.Now().UTC()
		state.Running = false
		state.FinishedAt = &now
		state.ExitCode = &exitCode
		state.LastError = c.classifyFailureLocked(exitCode, state.StopRequested)
		if err := c.writeStateLocked(state); err != nil {
			c.logger.Error("registration_state_write_failed", "error", err)
		}
		c.appendLogLocked(fmt.Sprintf("[website] 注册进程已退出: 退出码=%d", exitCode))
		c.releaseLockLocked()
	}
	if err != nil {
		c.logger.Info("registration_process_exited", "pid", command.Process.Pid, "exit_code", exitCode)
	}
	c.command = nil
	c.done = nil
	close(done)
}

func (c *Controller) statusLocked(state persistedState) Status {
	return Status{
		Configured: c.configuredLocked(), Running: state.Running, PID: state.PID,
		StartedAt: state.StartedAt, FinishedAt: state.FinishedAt, ExitCode: state.ExitCode,
		LastError: state.LastError, Progress: c.progressLocked(state),
	}
}

func (c *Controller) progressLocked(state persistedState) Progress {
	return c.protocolProgressLocked(state, countProtocolAccounts(c.protocolLedgerPath()))
}

func (c *Controller) protocolProgressLocked(state persistedState, accountCount int) Progress {
	done := 0
	total := 0
	progress := Progress{Mode: state.ProgressMode, AccountCount: accountCount}
	if data, err := os.ReadFile(c.protocolStatePath()); err == nil {
		var worker struct {
			Done      int  `json:"done"`
			Target    int  `json:"target"`
			Attempted *int `json:"attempted"`
			OK        int  `json:"ok"`
			Failed    int  `json:"failed"`
			Resumable int  `json:"resumable"`
		}
		if json.Unmarshal(data, &worker) == nil {
			done = max(0, worker.Done)
			total = max(0, worker.Target)
			if worker.Attempted != nil {
				progress.Attempted = max(0, *worker.Attempted)
				progress.Succeeded = max(0, worker.OK)
			} else if worker.OK > 0 || worker.Failed > 0 {
				// v1 worker used done for terminal attempts, not usable successes.
				progress.Attempted = done
				done = max(0, worker.OK)
				progress.Succeeded = done
			} else {
				progress.Attempted = done
				progress.Succeeded = done
			}
			progress.Failed = max(0, worker.Failed)
			progress.Resumable = max(0, worker.Resumable)
		}
	}
	if state.TargetCount != nil {
		total = max(0, *state.TargetCount)
	}
	if total > 0 {
		done = min(done, total)
	}
	progress.Done = done
	if total <= 0 {
		progress.Indeterminate = true
		return progress
	}
	progress.Total = &total
	percent := float64(0)
	if total > 0 {
		percent = min(100, float64(done)*100/float64(total))
	}
	progress.Percent = &percent
	return progress
}

func (c *Controller) reconcileStateLocked() (persistedState, error) {
	state, err := c.readStateLocked()
	if err != nil {
		return persistedState{}, err
	}
	managed := c.command != nil && c.command.Process != nil && c.command.Process.Pid == state.PID
	if state.Running && state.PID > 0 && !managed && !isRegistrationProcess(state.PID) {
		now := time.Now().UTC()
		code := -1
		state.Running = false
		state.FinishedAt = &now
		state.ExitCode = &code
		state.LastError = &Failure{Code: "registrationInterrupted", Message: "注册任务进程已退出"}
		if err := c.writeStateLocked(state); err != nil {
			return persistedState{}, err
		}
		c.releaseLockLocked()
	}
	return state, nil
}

func (c *Controller) classifyFailureLocked(exitCode int, stopped bool) *Failure {
	if exitCode == 0 {
		return nil
	}
	if stopped {
		return &Failure{Code: "registrationStopped", Message: "注册任务已停止"}
	}
	if exitCode == 3 {
		return &Failure{Code: "registrationPartial", Message: "注册任务仅部分完成，可从 checkpoint 继续"}
	}
	logs, _ := readLogEntries(c.logPath(), maxRegistrationLogLines)
	text := ""
	for _, entry := range logs {
		text += entry.Text + "\n"
	}
	switch {
	case strings.Contains(text, "TempMail.lol did not receive a verification email"):
		return &Failure{Code: "tempmailDeliveryTimeout", Message: "TempMail.lol 未收到 xAI 验证邮件"}
	case strings.Contains(text, "chat probe failed:") || exitCode == 2:
		return &Failure{Code: "cpaChatProbeFailed", Message: "账号注册完成，但 CPA 聊天能力探测未通过"}
	default:
		return &Failure{Code: "registrationFailed", Message: "注册任务执行失败，请查看运行日志"}
	}
}

func (c *Controller) configuredLocked() bool {
	if !c.config.Enabled {
		return false
	}
	if len(c.config.Command) == 0 || strings.TrimSpace(c.config.WorkDir) == "" || strings.TrimSpace(c.config.ConfigPath) == "" {
		return false
	}
	workInfo, err := os.Stat(c.config.WorkDir)
	if err != nil || !workInfo.IsDir() {
		return false
	}
	_, err = resolveCommand(c.config.Command)
	return err == nil
}

func (c *Controller) ensureWorkerConfigLocked() error {
	if len(c.config.Command) == 0 || !c.configuredLocked() {
		return ErrNotConfigured
	}
	if _, err := os.Stat(c.config.ConfigPath); errors.Is(err, os.ErrNotExist) {
		example := filepath.Join(c.config.WorkDir, "config.example.json")
		value, readErr := readJSONMap(example)
		if readErr != nil {
			return fmt.Errorf("读取注册配置模板: %w", readErr)
		}
		c.forceSafeWorkerSettings(value)
		return writeJSONAtomic(c.config.ConfigPath, value, 0o600)
	} else if err != nil {
		return fmt.Errorf("检查注册配置: %w", err)
	}
	value, err := readJSONMap(c.config.ConfigPath)
	if err != nil {
		return err
	}
	c.forceSafeWorkerSettings(value)
	return writeJSONAtomic(c.config.ConfigPath, value, 0o600)
}

func (c *Controller) forceSafeWorkerSettings(value map[string]any) {
	value["engine"] = "protocol"
	delete(value, "protocol_fallback_browser")
	if !isSupportedEmailProvider(stringValue(value["email_provider"], "")) {
		value["email_provider"] = "yyds"
	}
	fallbacks := make([]string, 0)
	for _, provider := range stringSlice(value["email_provider_fallbacks"]) {
		if isSupportedEmailProvider(provider) {
			fallbacks = append(fallbacks, provider)
		}
	}
	value["email_provider_fallbacks"] = fallbacks
	value["cpa_remote_import_enabled"] = false
	value["grok2api_auto_add_remote"] = false
	value["grok2api_auto_add_local"] = false
	value["cpa_copy_to_hotload"] = true
	value["cpa_hotload_await_result"] = true
	value["cpa_hotload_dir"] = filepath.Join(c.config.SpoolPath, "incoming")
	value["spool_dir"] = filepath.Join(c.config.SpoolPath, "incoming")
	value["cpa_auth_dir"] = filepath.Join(c.dataPath(), "cpa_auths")
	if endpoint, ok := os.LookupEnv("REGISTRATION_CAPTCHA_ENDPOINT"); ok {
		value["captcha_endpoint"] = strings.TrimSpace(endpoint)
	}
	if proxy, ok := os.LookupEnv("REGISTRATION_PROXY"); ok {
		value["proxy"] = strings.TrimSpace(proxy)
	}
	value["registration_config_version"] = 4
}

func (c *Controller) workerEnvironment() []string {
	environment := append([]string(nil), os.Environ()...)
	values := map[string]string{
		"PYTHONIOENCODING":                   "utf-8",
		"REGISTRATION_CONFIG_FILE":           c.config.ConfigPath,
		"REGISTRATION_DATA_DIR":              c.dataPath(),
		"REGISTRATION_CPA_EXPORT_DIR":        filepath.Join(c.dataPath(), "cpa_auths"),
		"REGISTRATION_CPA_HOTLOAD_DIR":       filepath.Join(c.config.SpoolPath, "incoming"),
		"REGISTRATION_DISABLE_REMOTE_IMPORT": "1",
	}
	if mode := strings.TrimSpace(c.config.BrowserMode); mode != "" {
		values["REGISTRATION_BROWSER_MODE"] = mode
	}
	if path := strings.TrimSpace(c.config.BrowserPath); path != "" {
		values["REGISTRATION_BROWSER_PATH"] = path
	}
	for key, value := range values {
		environment = setEnvironment(environment, key, value)
	}
	return environment
}

func (c *Controller) dataPath() string { return filepath.Dir(c.config.SpoolPath) }
func (c *Controller) statePath() string {
	return filepath.Join(c.dataPath(), "registration_state.json")
}
func (c *Controller) lockPath() string { return filepath.Join(c.dataPath(), "registration.lock") }
func (c *Controller) logPath() string  { return filepath.Join(c.dataPath(), "registration.log") }
func (c *Controller) protocolStatePath() string {
	return filepath.Join(c.dataPath(), "state.json")
}
func (c *Controller) protocolLedgerPath() string {
	return filepath.Join(c.dataPath(), "protocol_accounts.jsonl")
}

func (c *Controller) readStateLocked() (persistedState, error) {
	data, err := os.ReadFile(c.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return persistedState{}, nil
	}
	if err != nil {
		return persistedState{}, fmt.Errorf("读取注册任务状态: %w", err)
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return persistedState{}, fmt.Errorf("解析注册任务状态: %w", err)
	}
	return state, nil
}

func (c *Controller) writeStateLocked(state persistedState) error {
	return writeJSONAtomic(c.statePath(), state, 0o600)
}

func (c *Controller) acquireLockLocked() error {
	if err := ensurePrivateDirectory(c.dataPath()); err != nil {
		return err
	}
	file, err := os.OpenFile(c.lockPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		state, _ := c.readStateLocked()
		if state.PID > 0 && isRegistrationProcess(state.PID) {
			return ErrRunning
		}
		_ = os.Remove(c.lockPath())
		return c.acquireLockLocked()
	}
	if err != nil {
		return fmt.Errorf("获取注册任务锁: %w", err)
	}
	_, writeErr := fmt.Fprintf(file, "%d\n", os.Getpid())
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}

func (c *Controller) releaseLockLocked() { _ = os.Remove(c.lockPath()) }

func (c *Controller) resetLogsLocked() error {
	if err := ensurePrivateDirectory(c.dataPath()); err != nil {
		return err
	}
	return os.WriteFile(c.logPath(), nil, 0o600)
}

func (c *Controller) appendLogLocked(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	if info, err := os.Stat(c.logPath()); err == nil && info.Size() > maxRegistrationLogBytes {
		entries, _ := readLogEntries(c.logPath(), maxRegistrationLogLines)
		slices.Reverse(entries)
		var builder strings.Builder
		for _, entry := range entries {
			builder.WriteString(entry.Text)
			builder.WriteByte('\n')
		}
		_ = os.WriteFile(c.logPath(), []byte(builder.String()), 0o600)
	}
	file, err := os.OpenFile(c.logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		c.logger.Warn("registration_log_open_failed", "error", err)
		return
	}
	_, err = io.WriteString(file, truncateText(line, 1<<20)+"\n")
	_ = file.Close()
	if err != nil {
		c.logger.Warn("registration_log_write_failed", "error", err)
	}
}

func readLogEntries(path string, limit int) ([]LogEntry, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []LogEntry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取注册日志: %w", err)
	}
	defer file.Close()
	all := make([]LogEntry, 0, min(limit, maxRegistrationLogLines))
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var id uint64
	for scanner.Scan() {
		id++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		all = append(all, LogEntry{ID: id, Text: text})
		if len(all) > limit {
			all = all[1:]
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("扫描注册日志: %w", err)
	}
	slices.Reverse(all)
	return all, nil
}

func settingsView(value map[string]any) WorkerSettings {
	solver := strings.ToLower(strings.TrimSpace(stringValue(value["captcha_solver"], "local")))
	if solver != "yescaptcha" {
		solver = "local"
	}
	endpoint := strings.TrimSpace(stringValue(value["captcha_endpoint"], ""))
	if endpoint == "" {
		endpoint = strings.TrimSpace(stringValue(value["local_captcha_endpoint"], ""))
	}
	return WorkerSettings{
		Engine:                 "protocol",
		EmailSources:           emailSourcesView(value),
		EmailProvider:          stringValue(value["email_provider"], "yyds"),
		EmailProviderFallbacks: stringSlice(value["email_provider_fallbacks"]),
		TempmailLolAPIBase:     stringValue(value["tempmail_lol_api_base"], "https://api.tempmail.lol/v2"),
		TempmailLolDomain:      stringValue(value["tempmail_lol_domain"], ""),
		TempmailLolPrefix:      stringValue(value["tempmail_lol_prefix"], ""),
		Proxy:                  stringValue(value["proxy"], ""), CPABaseURL: stringValue(value["cpa_base_url"], "https://cli-chat-proxy.grok.com/v1"),
		CPAProxy: stringValue(value["cpa_proxy"], ""), CPAHeadless: boolValue(value["cpa_headless"], false),
		CPAProbeChat: boolValue(value["cpa_probe_chat"], true), CPACloseBrowserAfterAuth: boolValue(value["cpa_close_browser_after_auth"], true),
		CaptchaSolver: solver, CaptchaEndpoint: endpoint,
		YydsAPIKeyConfigured:    strings.TrimSpace(stringValue(value["yyds_api_key"], "")) != "",
		YydsJWTConfigured:       strings.TrimSpace(stringValue(value["yyds_jwt"], "")) != "",
		YesCaptchaKeyConfigured: strings.TrimSpace(stringValue(value["yescaptcha_api_key"], "")) != "",
	}
}

func applySettingsPatch(value map[string]any, patch WorkerSettingsPatch) error {
	if patch.Engine != nil {
		engine := strings.ToLower(strings.TrimSpace(*patch.Engine))
		if engine != "protocol" {
			return fmt.Errorf("%w: 不支持的注册引擎", ErrInvalidInput)
		}
	}
	value["engine"] = "protocol"
	if patch.CaptchaSolver != nil {
		solver := strings.ToLower(strings.TrimSpace(*patch.CaptchaSolver))
		if !slices.Contains([]string{"local", "yescaptcha"}, solver) {
			return fmt.Errorf("%w: 不支持的验证码方式", ErrInvalidInput)
		}
		value["captcha_solver"] = solver
	}
	if patch.CaptchaEndpoint != nil {
		endpoint, err := validateCaptchaEndpoint(*patch.CaptchaEndpoint)
		if err != nil {
			return fmt.Errorf("%w: 验证码 endpoint 无效", ErrInvalidInput)
		}
		value["captcha_endpoint"] = endpoint
	}
	for key, secret := range map[string]*string{
		"yyds_api_key":       patch.YydsAPIKey,
		"yyds_jwt":           patch.YydsJWT,
		"yescaptcha_api_key": patch.YesCaptchaAPIKey,
	} {
		if secret == nil || strings.TrimSpace(*secret) == "" {
			continue
		}
		trimmed := strings.TrimSpace(*secret)
		if len(trimmed) > 4096 {
			return fmt.Errorf("%w: 密钥过长", ErrInvalidInput)
		}
		value[key] = trimmed
	}
	if patch.EmailProvider != nil {
		provider := strings.TrimSpace(*patch.EmailProvider)
		if !isSupportedEmailProvider(provider) {
			return fmt.Errorf("%w: 不支持的邮箱服务", ErrInvalidInput)
		}
		value["email_provider"] = provider
	}
	if patch.EmailProviderFallbacks != nil {
		fallbacks := make([]string, 0, len(*patch.EmailProviderFallbacks))
		for _, provider := range *patch.EmailProviderFallbacks {
			provider = strings.TrimSpace(provider)
			if !isSupportedEmailProvider(provider) {
				return fmt.Errorf("%w: 邮箱回退服务无效", ErrInvalidInput)
			}
			fallbacks = append(fallbacks, provider)
		}
		value["email_provider_fallbacks"] = fallbacks
	}
	for key, pointer := range map[string]*string{
		"tempmail_lol_domain": patch.TempmailLolDomain, "tempmail_lol_prefix": patch.TempmailLolPrefix,
		"proxy": patch.Proxy, "cpa_proxy": patch.CPAProxy,
	} {
		if pointer != nil {
			trimmed := strings.TrimSpace(*pointer)
			if len(trimmed) > 2048 {
				return fmt.Errorf("%w: 配置值过长", ErrInvalidInput)
			}
			value[key] = trimmed
		}
	}
	if patch.TempmailLolAPIBase != nil {
		normalized, err := validateHTTPURL(*patch.TempmailLolAPIBase)
		if err != nil {
			return fmt.Errorf("%w: TempMail API 地址无效", ErrInvalidInput)
		}
		value["tempmail_lol_api_base"] = normalized
	}
	if patch.CPABaseURL != nil {
		normalized, err := validateCPABaseURL(*patch.CPABaseURL)
		if err != nil {
			return fmt.Errorf("%w: CPA 上游地址无效", ErrInvalidInput)
		}
		value["cpa_base_url"] = normalized
	}
	if patch.CPAHeadless != nil {
		value["cpa_headless"] = *patch.CPAHeadless
	}
	if patch.CPAProbeChat != nil {
		value["cpa_probe_chat"] = *patch.CPAProbeChat
	}
	if patch.CPACloseBrowserAfterAuth != nil {
		value["cpa_close_browser_after_auth"] = *patch.CPACloseBrowserAfterAuth
	}
	if patch.EmailSources != nil {
		if err := applyEmailSourcesPatch(value, *patch.EmailSources); err != nil {
			return err
		}
	}
	return nil
}

func isSupportedEmailProvider(provider string) bool {
	return slices.Contains([]string{"tempmail_lol", "yyds"}, strings.TrimSpace(provider))
}

func defaultEmailAPIBase(provider string) string {
	if provider == "tempmail_lol" {
		return "https://api.tempmail.lol"
	}
	return "https://maliapi.215.im/v1"
}

func readStoredEmailSources(value map[string]any) []storedEmailSource {
	if raw, ok := value["email_sources"]; ok {
		data, err := json.Marshal(raw)
		if err == nil {
			var sources []storedEmailSource
			if json.Unmarshal(data, &sources) == nil && len(sources) > 0 {
				return sources
			}
		}
	}

	providers := append([]string{stringValue(value["email_provider"], "yyds")}, stringSlice(value["email_provider_fallbacks"])...)
	sources := make([]storedEmailSource, 0, len(providers))
	for index, provider := range providers {
		provider = strings.TrimSpace(provider)
		if !isSupportedEmailProvider(provider) {
			continue
		}
		source := storedEmailSource{ID: fmt.Sprintf("source-%d", index+1), Type: provider, Enabled: true, APIBase: defaultEmailAPIBase(provider)}
		if provider == "tempmail_lol" {
			source.APIKey = stringValue(value["tempmail_api_key"], "")
			source.Domain = stringValue(value["tempmail_lol_domain"], "")
			source.Prefix = stringValue(value["tempmail_lol_prefix"], "")
			if configuredBase := strings.TrimSuffix(stringValue(value["tempmail_lol_api_base"], ""), "/v2"); configuredBase != "" {
				source.APIBase = configuredBase
			}
		} else {
			source.APIKey = stringValue(value["yyds_api_key"], "")
			source.JWT = stringValue(value["yyds_jwt"], "")
			if configuredBase := stringValue(value["yyds_api_base"], ""); configuredBase != "" {
				source.APIBase = configuredBase
			}
		}
		sources = append(sources, source)
	}
	if len(sources) == 0 {
		sources = append(sources, storedEmailSource{ID: "source-1", Type: "yyds", Enabled: true, APIBase: defaultEmailAPIBase("yyds")})
	}
	return sources
}

func emailSourcesView(value map[string]any) []EmailSourceSettings {
	stored := readStoredEmailSources(value)
	sources := make([]EmailSourceSettings, 0, len(stored))
	for _, source := range stored {
		if !isSupportedEmailProvider(source.Type) {
			continue
		}
		apiBase := strings.TrimSpace(source.APIBase)
		if apiBase == "" {
			apiBase = defaultEmailAPIBase(source.Type)
		}
		sources = append(sources, EmailSourceSettings{
			ID: source.ID, Type: source.Type, Enabled: source.Enabled, APIBase: apiBase,
			Domain: source.Domain, Prefix: source.Prefix,
			APIKeyConfigured: strings.TrimSpace(source.APIKey) != "",
			JWTConfigured:    strings.TrimSpace(source.JWT) != "",
		})
	}
	return sources
}

func applyEmailSourcesPatch(value map[string]any, patch []EmailSourceSettings) error {
	if len(patch) == 0 || len(patch) > 10 {
		return fmt.Errorf("%w: email sources must contain 1 to 10 items", ErrInvalidInput)
	}
	existing := make(map[string]storedEmailSource)
	for _, source := range readStoredEmailSources(value) {
		existing[source.ID] = source
	}

	seenIDs := make(map[string]struct{}, len(patch))
	seenTypes := make(map[string]struct{}, len(patch))
	stored := make([]storedEmailSource, 0, len(patch))
	enabledProviders := make([]string, 0, len(patch))
	for _, source := range patch {
		source.ID = strings.TrimSpace(source.ID)
		source.Type = strings.TrimSpace(source.Type)
		if source.ID == "" || len(source.ID) > 80 {
			return fmt.Errorf("%w: email source id is invalid", ErrInvalidInput)
		}
		if _, exists := seenIDs[source.ID]; exists {
			return fmt.Errorf("%w: duplicate email source id", ErrInvalidInput)
		}
		seenIDs[source.ID] = struct{}{}
		if !isSupportedEmailProvider(source.Type) {
			return fmt.Errorf("%w: unsupported email source type", ErrInvalidInput)
		}
		if _, exists := seenTypes[source.Type]; exists {
			return fmt.Errorf("%w: duplicate email source type", ErrInvalidInput)
		}
		seenTypes[source.Type] = struct{}{}
		apiBase := strings.TrimSpace(source.APIBase)
		if apiBase == "" {
			apiBase = defaultEmailAPIBase(source.Type)
		}
		normalizedBase, err := validateHTTPURL(apiBase)
		if err != nil {
			return fmt.Errorf("%w: email source API base is invalid", ErrInvalidInput)
		}
		if len(source.Domain) > 2048 || len(source.Prefix) > 128 {
			return fmt.Errorf("%w: email source configuration is too long", ErrInvalidInput)
		}
		current := existing[source.ID]
		secret := strings.TrimSpace(source.APIKey)
		jwt := strings.TrimSpace(source.JWT)
		if secret == "" && current.Type == source.Type {
			secret = current.APIKey
		}
		if jwt == "" && current.Type == source.Type {
			jwt = current.JWT
		}
		if len(secret) > 4096 || len(jwt) > 4096 {
			return fmt.Errorf("%w: email source secret is too long", ErrInvalidInput)
		}
		item := storedEmailSource{
			ID: source.ID, Type: source.Type, Enabled: source.Enabled, APIBase: normalizedBase,
			APIKey: secret, JWT: jwt, Domain: strings.TrimSpace(source.Domain), Prefix: strings.TrimSpace(source.Prefix),
		}
		stored = append(stored, item)
		if item.Enabled {
			enabledProviders = append(enabledProviders, item.Type)
		}
	}
	if len(enabledProviders) == 0 {
		return fmt.Errorf("%w: at least one email source must be enabled", ErrInvalidInput)
	}

	value["email_sources"] = stored
	value["email_provider"] = enabledProviders[0]
	value["email_provider_fallbacks"] = enabledProviders[1:]
	for _, source := range stored {
		switch source.Type {
		case "tempmail_lol":
			value["tempmail_api_key"] = source.APIKey
			value["tempmail_lol_api_base"] = strings.TrimRight(source.APIBase, "/") + "/v2"
			value["tempmail_lol_domain"] = source.Domain
			value["tempmail_lol_prefix"] = source.Prefix
		case "yyds":
			value["yyds_api_base"] = source.APIBase
			value["yyds_api_key"] = source.APIKey
			value["yyds_jwt"] = source.JWT
		}
	}
	return nil
}

func validateHTTPURL(raw string) (string, error) {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return "", errors.New("invalid HTTP URL")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func validateCaptchaEndpoint(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(strings.ToLower(value), "docker://") {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") {
			return "", errors.New("invalid docker endpoint")
		}
		return strings.TrimRight(value, "/"), nil
	}
	return validateHTTPURL(value)
}

func validateCPABaseURL(raw string) (string, error) {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("invalid CPA URL")
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "x.ai" && host != "grok.com" && !strings.HasSuffix(host, ".x.ai") && !strings.HasSuffix(host, ".grok.com") {
		return "", errors.New("CPA host is not allowed")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func proxyReady(raw string) (bool, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true, "direct"
	}
	value := raw
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return false, "invalid proxy"
	}
	port := parsed.Port()
	if port == "" {
		switch parsed.Scheme {
		case "https":
			port = "443"
		case "socks4", "socks4a", "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	label := parsed.Scheme + "://" + net.JoinHostPort(parsed.Hostname(), port)
	connection, err := net.DialTimeout("tcp", net.JoinHostPort(parsed.Hostname(), port), 3*time.Second)
	if err != nil {
		return false, label
	}
	connection.Close()
	return true, label
}

func resolveCommand(command []string) (string, error) {
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return "", ErrNotConfigured
	}
	if filepath.IsAbs(command[0]) || strings.ContainsAny(command[0], `/\\`) {
		info, err := os.Stat(command[0])
		if err != nil || info.IsDir() {
			return command[0], ErrNotConfigured
		}
		return command[0], nil
	}
	return exec.LookPath(command[0])
}

func protocolWorkerArguments(command []string, script string, arguments ...string) []string {
	prefix := append([]string(nil), command[1:]...)
	for _, value := range prefix {
		if filepath.Base(value) == filepath.Base(script) {
			return append(prefix, arguments...)
		}
	}
	if len(prefix) == 0 && isPythonCommand(command[0]) {
		return append([]string{"-u", script}, arguments...)
	}
	return append(append(prefix, "--protocol-worker"), arguments...)
}

func isPythonCommand(command string) bool {
	base := strings.ToLower(filepath.Base(command))
	return base == "python" || base == "python3" || base == "py" || strings.HasPrefix(base, "python3.")
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	if err := ensurePrivateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("编码 JSON: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".registration-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("提交 JSON: %w", err)
	}
	return nil
}

func readJSONMap(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	value := map[string]any{}
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("创建目录 %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("保护目录 %s: %w", path, err)
	}
	return nil
}

func countNonEmptyLines(path string) int {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	count := 0
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			count++
		}
	}
	return count
}

func countProtocolAccounts(path string) int {
	file, err := os.Open(path)
	if err == nil {
		defer file.Close()
		scanner := bufio.NewScanner(file)
		count := 0
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var row map[string]json.RawMessage
			if json.Unmarshal([]byte(line), &row) == nil && len(row) > 0 {
				count++
			}
		}
		return count
	}
	legacyPath := strings.TrimSuffix(path, "l")
	data, legacyErr := os.ReadFile(legacyPath)
	if legacyErr != nil {
		return 0
	}
	var rows []json.RawMessage
	if json.Unmarshal(data, &rows) != nil {
		return 0
	}
	return len(rows)
}

func setEnvironment(environment []string, key, value string) []string {
	prefix := strings.ToUpper(key) + "="
	for index, item := range environment {
		if strings.HasPrefix(strings.ToUpper(item), prefix) {
			environment[index] = key + "=" + value
			return environment
		}
	}
	return append(environment, key+"="+value)
}

func stringValue(value any, fallback string) string {
	if text, ok := value.(string); ok {
		return text
	}
	return fallback
}
func boolValue(value any, fallback bool) bool {
	if result, ok := value.(bool); ok {
		return result
	}
	return fallback
}
func stringSlice(value any) []string {
	items, ok := value.([]any)
	if !ok {
		if strings, ok := value.([]string); ok {
			return append([]string(nil), strings...)
		}
		return []string{}
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}
func truncateText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
