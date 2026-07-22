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
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxRegistrationLogLines    = 500
	maxRegistrationLogBytes    = 5 << 20
	protocolPreflightTimeout   = 20 * time.Second
	browserPreflightTimeout    = 75 * time.Second
	registrationEngineProtocol = "protocol"
	registrationEngineBrowser  = "browser"
	defaultCaptchaEndpoint     = "http://grok-turnstile-solver:5072"
	defaultBrowserSignupURL    = "https://accounts.x.ai/sign-up?redirect=grok-com"
	defaultBrowserEgressURL    = "https://api.ipify.org?format=json"
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
	Configured          bool       `json:"configured"`
	Running             bool       `json:"running"`
	PID                 int        `json:"pid,omitempty"`
	StartedAt           *time.Time `json:"startedAt,omitempty"`
	FinishedAt          *time.Time `json:"finishedAt,omitempty"`
	ExitCode            *int       `json:"exitCode,omitempty"`
	LastError           *Failure   `json:"lastError,omitempty"`
	DurationMs          *int64     `json:"durationMs,omitempty"`
	AveragePerAccountMs *int64     `json:"averagePerAccountMs,omitempty"`
	Progress            Progress   `json:"progress"`
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

type workerPreflightCheck struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type workerPreflightPayload struct {
	Checks map[string]workerPreflightCheck `json:"checks"`
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
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Enabled          bool            `json:"enabled"`
	APIBase          string          `json:"apiBase"`
	APIKey           string          `json:"apiKey"`
	JWT              string          `json:"jwt"`
	Domain           string          `json:"domain"`
	Prefix           string          `json:"prefix"`
	APIKeyConfigured bool            `json:"apiKeyConfigured"`
	JWTConfigured    bool            `json:"jwtConfigured"`
	Options          map[string]any  `json:"options"`
	OptionConfigured map[string]bool `json:"optionConfigured"`
}

type storedEmailSource struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Enabled bool           `json:"enabled"`
	APIBase string         `json:"api_base,omitempty"`
	APIKey  string         `json:"api_key,omitempty"`
	JWT     string         `json:"jwt,omitempty"`
	Domain  string         `json:"domain,omitempty"`
	Prefix  string         `json:"prefix,omitempty"`
	Options map[string]any `json:"options,omitempty"`
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
	ProxyGroupID             string                `json:"proxyGroupId"`
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
	ProxyGroupID             *string                `json:"proxyGroupId"`
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
	engine, err := c.engineLocked()
	if err != nil {
		return Status{}, err
	}
	if err := c.resolveProxyGroupLocked(ctx, registrationProxyScope(accountType)); err != nil {
		return Status{}, err
	}
	preflight := c.preflightLocked(ctx)
	if !preflight.OK {
		c.appendPreflightLogLocked(preflight)
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
	c.appendPreflightLogLocked(preflight)

	mode := "count"
	value := input.Count
	if input.Extra > 0 {
		mode = "extra"
		value = input.Extra
	} else if input.Count == 0 {
		value = 1
	}
	target := &value
	if err := os.Remove(c.workerStatePath(engine)); err != nil && !errors.Is(err, os.ErrNotExist) {
		c.releaseLockLocked()
		return Status{}, fmt.Errorf("clear %s registration state: %w", engine, err)
	}

	workerScript := c.workerScriptPath(engine)
	if _, err := os.Stat(workerScript); err != nil {
		c.releaseLockLocked()
		return Status{}, fmt.Errorf("%s registration script does not exist: %w", engine, err)
	}
	var arguments []string
	if engine == registrationEngineBrowser {
		arguments = browserWorkerArguments(c.config.Command, workerScript,
			"--config", c.config.ConfigPath,
			"--state-dir", c.dataPath(),
			"--log-file", c.logPath(),
			"--accounts-file", c.browserLedgerPath(),
			"--count", strconv.Itoa(input.Count),
			"--threads", strconv.Itoa(input.Threads),
			"--account-type", accountType,
		)
		if accountType == "build" {
			arguments = append(arguments, "--inline-mint")
		}
		if effectiveProxy != "" {
			arguments = append(arguments, "--proxy", effectiveProxy)
		}
	} else {
		arguments = protocolWorkerArguments(c.config.Command, workerScript,
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
	c.appendLogLocked(fmt.Sprintf("[website] starting %s registration: account_type=%s count=%d extra=%d threads=%d", engine, accountType, input.Count, input.Extra, input.Threads))
	command.Dir = c.config.WorkDir
	command.Env = c.workerEnvironmentForEngine(engine)
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
		Engine: engine, Running: true, PID: command.Process.Pid, StartedAt: &now,
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
	result := c.preflightLocked(ctx)
	c.appendPreflightLogLocked(result)
	return result
}

func (c *Controller) appendPreflightLogLocked(result PreflightResult) {
	status := "失败"
	if result.OK {
		status = "通过"
	}
	c.appendLogLocked(fmt.Sprintf("[预检] 注册 worker 预检%s", status))
	for _, check := range result.Checks {
		checkStatus := "失败"
		if check.OK {
			checkStatus = "通过"
		}
		c.appendLogLocked(fmt.Sprintf("[预检] %s：%s（%s）", preflightCheckLabel(check.Name), preflightDetailLabel(check.Detail), checkStatus))
	}
}

func preflightDetailLabel(detail string) string {
	labels := map[string]string{
		"ready": "已就绪", "protocol": "协议模式", "local-http": "本地 HTTP 服务",
		"worker unavailable": "Worker 不可用", "protocol mode skips browser CPA probe": "协议模式不执行浏览器 CPA 探测",
		"protocol mode skips cpaProxy":                 "协议模式不使用 CPA 代理",
		"API key or YYDS JWT for every enabled source": "每个启用邮件来源都需要 API Key 或 YYDS JWT",
		"yescaptcha_api_key":                           "需要配置 YesCaptcha API Key",
	}
	if label, ok := labels[detail]; ok {
		return label
	}
	return detail
}

func preflightCheckLabel(name string) string {
	if strings.HasPrefix(name, "emailAPI:") {
		return "邮件 API 地址（" + strings.TrimPrefix(name, "emailAPI:") + "）"
	}
	labels := map[string]string{
		"enabled": "功能开关", "workdir": "工作目录", "worker": "Worker 命令", "config": "Worker 配置",
		"registrationData": "注册数据目录", "spool": "任务队列目录", "engine": "注册引擎", "emailSources": "邮件来源",
		"emailCredentials": "邮件凭据", "cpaBaseURL": "CPA 地址", "proxy": "代理", "cpaProxy": "CPA 代理",
		"captchaEndpoint": "清障服务地址", "captchaSolver": "清障方案", "yescaptcha": "YesCaptcha 配置",
		"protocolWorker": "协议 Worker", "browserRuntime": "浏览器运行环境", "dependencies": "协议依赖", "egressIP": "出口 IP",
	}
	if label, ok := labels[name]; ok {
		return label
	}
	return name
}

func (c *Controller) preflightLocked(ctx context.Context) PreflightResult {
	if !c.config.Enabled {
		return PreflightResult{
			OK:     false,
			Checks: []PreflightCheck{{Name: "enabled", OK: false, Detail: "registration worker is disabled"}},
		}
	}
	checks := make([]PreflightCheck, 0, 20)
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
	add("spool", spoolErr == nil && directoryWritable(filepath.Join(c.config.SpoolPath, "incoming")), c.config.SpoolPath)
	add("logDirectory", ensurePrivateDirectory(c.dataPath()) == nil, c.logPath())

	// Keep the response schema valid even when the worker config cannot be
	// loaded. The preflight endpoint intentionally returns 200 with `ok:false`
	// so the UI can render individual checks; nil slices would serialize as
	// `null` and make the frontend reject the otherwise useful response.
	settings := WorkerSettings{
		EmailSources:           []EmailSourceSettings{},
		EmailProviderFallbacks: []string{},
	}
	configValue := map[string]any{}
	if configErr == nil {
		configValue, _ = readJSONMap(c.config.ConfigPath)
		settings = settingsView(configValue)
	}
	engine, engineErr := normalizeRegistrationEngine(settings.Engine)
	add("engine", configErr == nil && engineErr == nil, settings.Engine)
	if engineErr != nil {
		engine = registrationEngineProtocol
	}
	enabledSources := make([]EmailSourceSettings, 0, len(settings.EmailSources))
	credentialsReady := true
	for _, source := range settings.EmailSources {
		if !source.Enabled {
			continue
		}
		enabledSources = append(enabledSources, source)
		if engine == registrationEngineProtocol && !isProtocolEmailProvider(source.Type) {
			credentialsReady = false
		}
		if (source.Type == "yyds" || source.Type == "yyds_mail") && !source.APIKeyConfigured && !source.JWTConfigured {
			credentialsReady = false
		}
	}
	providerNames := make([]string, 0, len(enabledSources))
	for _, source := range enabledSources {
		providerNames = append(providerNames, source.Type)
	}
	add("emailSources", len(enabledSources) > 0, strings.Join(providerNames, ", "))
	add("emailCredentials", len(enabledSources) > 0 && credentialsReady, "configured credentials for every enabled source")
	for _, source := range enabledSources {
		apiBase := strings.TrimSpace(source.APIBase)
		if source.Type == "outlook_token" && apiBase == "" {
			add("emailAPI:"+source.ID, true, "Outlook Graph/IMAP does not require an API base")
			continue
		}
		_, apiErr := validateHTTPURL(apiBase)
		add("emailAPI:"+source.ID, apiErr == nil, apiBase)
	}
	// 协议路径不依赖浏览器 CPA 探测地址；仅校验 proxy 可选性。
	if settings.CPABaseURL != "" {
		_, cpaErr := validateCPABaseURL(settings.CPABaseURL)
		add("cpaBaseURL", cpaErr == nil, settings.CPABaseURL)
	} else {
		add("cpaBaseURL", true, "协议模式不执行浏览器 CPA 探测")
	}
	effectiveProxy, proxyOK, proxyDetail := resolveRegistrationProxy(settings.Proxy)
	add("proxy", proxyOK, proxyDetail)
	if strings.TrimSpace(settings.CPAProxy) == "" {
		add("cpaProxy", true, "未单独配置，沿用注册代理")
	} else {
		cpaProxyOK, cpaProxyDetail := proxyReady(settings.CPAProxy)
		add("cpaProxy", cpaProxyOK, cpaProxyDetail)
	}
	cpaDir := filepath.Join(c.dataPath(), "cpa_auths")
	add("cpaAuthDir", ensurePrivateDirectory(cpaDir) == nil, cpaDir)
	workerScript := c.workerScriptPath(engine)
	if engine == registrationEngineBrowser {
		browserModule := filepath.Join(c.config.WorkDir, "grok_register_ttk.py")
		manifest := filepath.Join(c.config.WorkDir, "turnstilePatch", "manifest.json")
		contentScript := filepath.Join(c.config.WorkDir, "turnstilePatch", "content.js")
		browserPath, browserErr := resolveBrowserExecutable(c.config.BrowserPath)
		displayOK, displayDetail := browserDisplayReady(c.workerEnvironmentForEngine(engine))
		browserRuntimeOK := regularFileReady(workerScript) && regularFileReady(browserModule) &&
			regularFileReady(manifest) && regularFileReady(contentScript) && browserErr == nil && displayOK
		browserRuntimeDetail := "ready"
		if !browserRuntimeOK {
			browserRuntimeDetail = "browser runtime unavailable; deploy compose.browser-registration.yml with the matching *-browser image"
		}
		add("browserRuntime", browserRuntimeOK, browserRuntimeDetail)
		addRegularFileCheck(add, "browserWorker", workerScript)
		addRegularFileCheck(add, "browserModule", browserModule)
		addRegularFileCheck(add, "turnstileManifest", manifest)
		addRegularFileCheck(add, "turnstileContent", contentScript)
		browserDetail := browserPath
		if browserErr != nil {
			browserDetail = browserErr.Error()
		}
		add("chromium", browserErr == nil, browserDetail)
		add("display", displayOK, displayDetail)
		add("cpaAuthWritable", directoryWritable(cpaDir), cpaDir)
		add("oauthConfig", browserOAuthConfigReady(configValue), "inline Build OAuth and CPA spool")
		proxyAuthOK, proxyAuthDetail := browserProxyAuthenticationReady(effectiveProxy)
		add("browserProxyAuth", proxyAuthOK, proxyAuthDetail)
		cpaBrowserProxy := effectiveProxy
		if strings.TrimSpace(settings.CPAProxy) != "" {
			resolved, ok, _ := resolveRegistrationProxy(settings.CPAProxy)
			if ok {
				cpaBrowserProxy = resolved
			}
		}
		cpaProxyAuthOK, cpaProxyAuthDetail := browserProxyAuthenticationReady(cpaBrowserProxy)
		add("cpaBrowserProxyAuth", cpaProxyAuthOK, cpaProxyAuthDetail)
		egressURL := strings.TrimSpace(os.Getenv("REGISTRATION_PREFLIGHT_EGRESS_URL"))
		if egressURL == "" {
			egressURL = strings.TrimSpace(stringValue(configValue["egress_check_url"], defaultBrowserEgressURL))
		}
		egressOK, egressDetail := probeEgressIP(ctx, egressURL, effectiveProxy)
		add("egressIP", proxyOK && egressOK, egressDetail)

		signupURL := strings.TrimSpace(os.Getenv("REGISTRATION_PREFLIGHT_SIGNUP_URL"))
		if signupURL == "" {
			signupURL = strings.TrimSpace(stringValue(configValue["signup_url"], defaultBrowserSignupURL))
		}
		reachable, detail := probeBrowserRegistrationPage(ctx, signupURL, effectiveProxy)
		add("registrationPage", proxyOK && reachable, detail)
		for _, source := range enabledSources {
			target := browserEmailProbeURL(source)
			reachable, detail := probeHTTPReachability(ctx, target, effectiveProxy)
			add("emailReachability:"+source.ID, reachable, detail)
		}
	} else {
		solver := strings.ToLower(strings.TrimSpace(stringValue(configValue["clearance_provider"], stringValue(configValue["captcha_solver"], "docker"))))
		if solver == "docker" || solver == "" {
			solver = "local"
		}
		yesKey := strings.TrimSpace(stringValue(configValue["yescaptcha_api_key"], ""))
		if yesKey == "" {
			yesKey = strings.TrimSpace(stringValue(configValue["yes_captcha_key"], ""))
		}
		if yesKey == "" {
			yesKey = strings.TrimSpace(stringValue(configValue["captcha_api_key"], ""))
		}
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
		addRegularFileCheck(add, "protocolWorker", workerScript)
		add("oauthConfig", oauthConfigReady(configValue), "OAuth authorization and import configuration")
	}

	dependencyOK := false
	dependencyDetail := "worker unavailable"
	workerChecks := map[string]workerPreflightCheck{}
	if commandErr == nil && workErr == nil {
		probeTimeout := protocolPreflightTimeout
		if engine == registrationEngineBrowser {
			probeTimeout = browserPreflightTimeout
		}
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		probeArguments := []string{
			"--preflight", "--config", c.config.ConfigPath,
			"--state-dir", c.dataPath(), "--log-file", c.logPath(),
		}
		arguments := protocolWorkerArguments(c.config.Command, workerScript, probeArguments...)
		if engine == registrationEngineBrowser {
			arguments = browserWorkerArguments(c.config.Command, workerScript, probeArguments...)
		}
		probe := exec.CommandContext(probeCtx, c.config.Command[0], arguments...)
		probe.Dir = c.config.WorkDir
		probe.Env = c.workerEnvironmentForEngine(engine)
		output, err := probe.CombinedOutput()
		if engine == registrationEngineBrowser {
			workerChecks = parseWorkerPreflightChecks(output)
		}
		dependencyOK = err == nil
		dependencyDetail = "ready"
		if err != nil {
			dependencyDetail = strings.TrimSpace(string(output))
			if errors.Is(probeCtx.Err(), context.Canceled) {
				dependencyDetail = "worker preflight canceled"
			} else if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
				dependencyDetail = fmt.Sprintf("worker preflight timed out after %s", probeTimeout)
			}
			if dependencyDetail == "" {
				dependencyDetail = err.Error()
			}
		}
	}
	if engine == registrationEngineBrowser {
		workerDetail := dependencyDetail
		if len(workerChecks) > 0 {
			workerDetail = workerPreflightFailureDetail(workerChecks)
			if dependencyOK && workerDetail == "" {
				workerDetail = "ready"
			} else if workerDetail == "" {
				workerDetail = "browser worker preflight failed"
			}
		}
		add("dependencies", dependencyOK, truncateText(workerDetail, 500))
		addWorkerPreflightCheck := func(name, workerName string) {
			check, ok := workerChecks[workerName]
			if !ok {
				add(name, dependencyOK, truncateText(dependencyDetail, 500))
				return
			}
			add(name, check.OK, truncateText(check.Detail, 500))
		}
		addWorkerPreflightCheck("grokRegisterImport", "grok_register_ttk")
		addWorkerPreflightCheck("drissionPage", "DrissionPage")
		addWorkerPreflightCheck("browserChromium", "chromium")
		addWorkerPreflightCheck("browserDisplay", "display")
		addWorkerPreflightCheck("browserRegistrationPage", "registrationPage")
	} else {
		add("dependencies", dependencyOK, truncateText(dependencyDetail, 500))
	}
	result := PreflightResult{OK: true, Checks: checks, Config: settings}
	for _, check := range checks {
		result.OK = result.OK && check.OK
	}
	return result
}

func workerPreflightFailureDetail(checks map[string]workerPreflightCheck) string {
	failures := make([]string, 0)
	for name, check := range checks {
		if check.OK {
			continue
		}
		detail := strings.TrimSpace(check.Detail)
		if detail == "" {
			detail = "failed"
		}
		failures = append(failures, name+": "+detail)
	}
	if len(failures) == 0 {
		return ""
	}
	sort.Strings(failures)
	return "browser worker preflight failed: " + strings.Join(failures, "; ")
}

func parseWorkerPreflightChecks(output []byte) map[string]workerPreflightCheck {
	lines := strings.Split(string(output), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var payload workerPreflightPayload
		if json.Unmarshal([]byte(line), &payload) == nil && payload.Checks != nil {
			return payload.Checks
		}
	}
	return nil
}

func oauthConfigReady(config map[string]any) bool {
	if len(config) == 0 {
		return false
	}
	// The protocol worker performs the import and endpoint checks. This guard
	// catches an accidentally replaced config before spawning that worker.
	return strings.TrimSpace(stringValue(config["engine"], "protocol")) == "protocol"
}

func browserOAuthConfigReady(config map[string]any) bool {
	if !boolValue(config["cpa_export_enabled"], true) {
		return false
	}
	_, err := validateCPABaseURL(stringValue(config["cpa_base_url"], "https://cli-chat-proxy.grok.com/v1"))
	return err == nil
}

func addRegularFileCheck(add func(string, bool, string), name, path string) {
	add(name, regularFileReady(path), path)
}

func regularFileReady(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func directoryWritable(path string) bool {
	if err := ensurePrivateDirectory(path); err != nil {
		return false
	}
	file, err := os.CreateTemp(path, ".registration-write-check-*")
	if err != nil {
		return false
	}
	name := file.Name()
	if closeErr := file.Close(); closeErr != nil {
		_ = os.Remove(name)
		return false
	}
	return os.Remove(name) == nil
}

func resolveBrowserExecutable(configured string) (string, error) {
	candidate := strings.TrimSpace(configured)
	if candidate == "" {
		candidate = strings.TrimSpace(os.Getenv("REGISTRATION_BROWSER_PATH"))
	}
	if candidate != "" {
		path, err := filepath.Abs(candidate)
		if err == nil {
			candidate = path
		}
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() {
			return candidate, nil
		}
		return "", fmt.Errorf("configured browser executable is unavailable: %s", candidate)
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	if runtime.GOOS == "windows" {
		for _, base := range []string{os.Getenv("PROGRAMFILES"), os.Getenv("PROGRAMFILES(X86)"), os.Getenv("LOCALAPPDATA")} {
			if base == "" {
				continue
			}
			for _, relative := range []string{
				filepath.Join("Google", "Chrome", "Application", "chrome.exe"),
			} {
				candidate := filepath.Join(base, relative)
				if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
					return candidate, nil
				}
			}
		}
	}
	return "", errors.New("Chromium or Chrome executable was not found")
}

func browserDisplayReady(environment []string) (bool, string) {
	if runtime.GOOS != "linux" {
		return true, runtime.GOOS + " does not require DISPLAY preflight"
	}
	mode := strings.ToLower(strings.TrimSpace(workerEnvironmentValue(environment, "REGISTRATION_BROWSER_MODE")))
	if mode == "headless" {
		return true, "headless"
	}
	if display := strings.TrimSpace(workerEnvironmentValue(environment, "DISPLAY")); display != "" {
		return true, "DISPLAY=" + display
	}
	if path, err := exec.LookPath("Xvfb"); err == nil {
		return true, path
	}
	return false, "DISPLAY is empty and Xvfb is unavailable"
}

func browserEmailProbeURL(source EmailSourceSettings) string {
	base := strings.TrimRight(strings.TrimSpace(source.APIBase), "/")
	if source.Type == "yyds" && !strings.HasSuffix(base, "/domains") {
		return base + "/domains"
	}
	return base
}

func browserProxyAuthenticationReady(proxy string) (bool, string) {
	if strings.TrimSpace(proxy) == "" {
		return true, "direct connection"
	}
	parsed, err := url.Parse(proxy)
	if err != nil {
		return false, "invalid proxy URL"
	}
	if parsed.User != nil {
		scheme := strings.ToLower(parsed.Scheme)
		if scheme != "http" && scheme != "https" {
			return false, "authenticated SOCKS proxy requires a local unauthenticated relay"
		}
		return true, "authenticated " + scheme + " proxy via Chromium auth extension"
	}
	return true, parsed.Scheme + "://" + parsed.Host
}

func probeHTTPReachability(ctx context.Context, target, proxy string) (bool, string) {
	targetURL, err := url.ParseRequestURI(strings.TrimSpace(target))
	if err != nil || targetURL.Scheme == "" || targetURL.Host == "" {
		return false, "invalid URL"
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if strings.TrimSpace(proxy) != "" {
		proxyURL, parseErr := url.Parse(proxy)
		if parseErr != nil {
			return false, "invalid proxy URL"
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, targetURL.String(), nil)
	if err != nil {
		return false, "request setup failed"
	}
	request.Header.Set("User-Agent", "Mozilla/5.0 Grok2API-Registration-Preflight")
	response, err := (&http.Client{Transport: transport, Timeout: 12 * time.Second}).Do(request)
	if err != nil {
		detail := err.Error()
		if proxy != "" {
			detail = strings.ReplaceAll(detail, proxy, "<registration-proxy>")
		}
		return false, targetURL.Host + ": " + truncateText(detail, 240)
	}
	defer response.Body.Close()
	_, _ = io.CopyN(io.Discard, response.Body, 1024)
	ok := response.StatusCode < http.StatusInternalServerError
	return ok, targetURL.Host + ": " + response.Status
}

// probeBrowserRegistrationPage is intentionally stricter than the generic
// reachability probe. A 4xx response (especially Cloudflare's challenge page)
// proves that the TCP route exists, but it is not a usable browser-registration
// route and would otherwise leave a worker retrying a page with no sign-up UI.
func probeBrowserRegistrationPage(ctx context.Context, target, proxy string) (bool, string) {
	targetURL, err := url.ParseRequestURI(strings.TrimSpace(target))
	if err != nil || targetURL.Scheme == "" || targetURL.Host == "" {
		return false, "invalid URL"
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if strings.TrimSpace(proxy) != "" {
		proxyURL, parseErr := url.Parse(proxy)
		if parseErr != nil {
			return false, "invalid proxy URL"
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, targetURL.String(), nil)
	if err != nil {
		return false, "request setup failed"
	}
	request.Header.Set("User-Agent", "Mozilla/5.0 Grok2API-Registration-Preflight")
	response, err := (&http.Client{Transport: transport, Timeout: 12 * time.Second}).Do(request)
	if err != nil {
		detail := err.Error()
		if proxy != "" {
			detail = strings.ReplaceAll(detail, proxy, "<registration-proxy>")
		}
		return false, targetURL.Host + ": " + truncateText(detail, 240)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 16*1024))
	detail := targetURL.Host + ": " + response.Status
	if response.StatusCode >= http.StatusBadRequest {
		return false, detail
	}
	page := strings.ToLower(string(body))
	if strings.EqualFold(strings.TrimSpace(response.Header.Get("cf-mitigated")), "challenge") ||
		strings.Contains(page, "attention required! | cloudflare") ||
		strings.Contains(page, "/cdn-cgi/challenge-platform/") {
		return false, detail + " (Cloudflare challenge)"
	}
	return true, detail
}

func probeEgressIP(ctx context.Context, target, proxy string) (bool, string) {
	targetURL, err := url.ParseRequestURI(strings.TrimSpace(target))
	if err != nil || targetURL.Scheme == "" || targetURL.Host == "" {
		return false, "invalid URL"
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if strings.TrimSpace(proxy) != "" {
		proxyURL, parseErr := url.Parse(proxy)
		if parseErr != nil {
			return false, "invalid proxy URL"
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, targetURL.String(), nil)
	if err != nil {
		return false, "request setup failed"
	}
	request.Header.Set("User-Agent", "Mozilla/5.0 Grok2API-Registration-Preflight")
	response, err := (&http.Client{Transport: transport, Timeout: 12 * time.Second}).Do(request)
	if err != nil {
		detail := err.Error()
		if proxy != "" {
			detail = strings.ReplaceAll(detail, proxy, "<registration-proxy>")
		}
		return false, targetURL.Host + ": " + truncateText(detail, 240)
	}
	defer response.Body.Close()
	if response.StatusCode >= http.StatusInternalServerError {
		return false, targetURL.Host + ": " + response.Status
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return false, targetURL.Host + ": unreadable response"
	}
	candidate := ""
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		candidate = strings.TrimSpace(stringValue(payload["ip"], ""))
	}
	if candidate == "" {
		for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "ip=") {
				candidate = strings.TrimSpace(strings.TrimPrefix(line, "ip="))
				break
			}
			if candidate == "" {
				candidate = line
			}
		}
	}
	if net.ParseIP(candidate) == nil {
		return false, targetURL.Host + ": response did not contain an IP address"
	}
	return true, candidate + " via " + targetURL.Host
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
	progress := c.progressLocked(state)
	status := Status{
		Configured: c.configuredLocked(), Running: state.Running, PID: state.PID,
		StartedAt: state.StartedAt, FinishedAt: state.FinishedAt, ExitCode: state.ExitCode,
		LastError: state.LastError, Progress: progress,
	}
	status.DurationMs, status.AveragePerAccountMs = registrationTimingMetrics(state, progress)
	return status
}

func registrationTimingMetrics(state persistedState, progress Progress) (*int64, *int64) {
	if state.StartedAt == nil {
		return nil, nil
	}
	end := time.Now().UTC()
	if state.FinishedAt != nil {
		end = *state.FinishedAt
	}
	duration := end.Sub(*state.StartedAt)
	if duration < 0 {
		duration = 0
	}
	durationMs := duration.Milliseconds()
	completedAccounts := progress.Succeeded
	if completedAccounts <= 0 {
		completedAccounts = progress.Done
	}
	if completedAccounts <= 0 {
		return &durationMs, nil
	}
	averagePerAccountMs := durationMs / int64(completedAccounts)
	return &durationMs, &averagePerAccountMs
}

func (c *Controller) progressLocked(state persistedState) Progress {
	if normalizedRegistrationEngine(state.Engine) == registrationEngineBrowser {
		return c.browserProgressLocked(state, countNonEmptyLines(c.browserLedgerPath()))
	}
	return c.protocolProgressLocked(state, countProtocolAccounts(c.protocolLedgerPath()))
}

func (c *Controller) protocolProgressLocked(state persistedState, accountCount int) Progress {
	return c.workerProgressLocked(state, accountCount, c.protocolStatePath())
}

func (c *Controller) browserProgressLocked(state persistedState, accountCount int) Progress {
	return c.workerProgressLocked(state, accountCount, c.browserStatePath())
}

func (c *Controller) workerProgressLocked(state persistedState, accountCount int, statePath string) Progress {
	done := 0
	total := 0
	progress := Progress{Mode: state.ProgressMode, AccountCount: accountCount}
	if data, err := os.ReadFile(statePath); err == nil {
		var worker struct {
			Done       int  `json:"done"`
			Target     int  `json:"target"`
			Attempted  *int `json:"attempted"`
			OK         int  `json:"ok"`
			Failed     int  `json:"failed"`
			Resumable  int  `json:"resumable"`
			Registered int  `json:"registered"`
		}
		if json.Unmarshal(data, &worker) == nil {
			progress.AccountCount = max(progress.AccountCount, worker.Registered)
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
	case strings.Contains(text, "chat probe failed:"):
		return &Failure{Code: "cpaChatProbeFailed", Message: "账号注册完成，但 CPA 聊天能力探测未通过"}
	case exitCode == 2:
		return &Failure{Code: "cpaMintIncomplete", Message: "账号注册完成，但 CPA 凭据生成或导入未完成"}
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
	engine, err := normalizeRegistrationEngine(stringValue(value["engine"], registrationEngineProtocol))
	if err != nil {
		engine = registrationEngineProtocol
	}
	value["engine"] = engine
	delete(value, "protocol_fallback_browser")
	delete(value, "protocol_email_backend")
	if !isSupportedEmailProvider(stringValue(value["email_provider"], "")) {
		value["email_provider"] = "tempmail_lol"
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
		value["clearance_endpoint"] = strings.TrimSpace(endpoint)
	} else if strings.EqualFold(strings.TrimSpace(stringValue(value["captcha_endpoint"], "")), "docker://grokcli-2api:5072") {
		value["captcha_endpoint"] = defaultCaptchaEndpoint
		value["clearance_endpoint"] = defaultCaptchaEndpoint
	}
	if strings.TrimSpace(stringValue(value["clearance_provider"], "")) == "" {
		if strings.ToLower(strings.TrimSpace(stringValue(value["captcha_solver"], "local"))) == "yescaptcha" {
			value["clearance_provider"] = "yescaptcha"
		} else {
			value["clearance_provider"] = "docker"
		}
	}
	if proxy, ok := os.LookupEnv("REGISTRATION_PROXY"); ok {
		value["proxy"] = strings.TrimSpace(proxy)
	}
	value["registration_config_version"] = 5
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

func (c *Controller) workerEnvironmentForEngine(engine string) []string {
	environment := c.workerEnvironment()
	if engine == registrationEngineBrowser && workerEnvironmentValue(environment, "REGISTRATION_BROWSER_MODE") == "" {
		environment = setEnvironment(environment, "REGISTRATION_BROWSER_MODE", "headless")
	}
	return environment
}

func workerEnvironmentValue(environment []string, key string) string {
	prefix := strings.ToUpper(key) + "="
	for _, item := range environment {
		if strings.HasPrefix(strings.ToUpper(item), prefix) {
			return item[len(prefix):]
		}
	}
	return ""
}

func (c *Controller) engineLocked() (string, error) {
	value, err := readJSONMap(c.config.ConfigPath)
	if err != nil {
		return "", err
	}
	return normalizeRegistrationEngine(stringValue(value["engine"], registrationEngineProtocol))
}

func (c *Controller) workerScriptPath(engine string) string {
	if engine == registrationEngineBrowser {
		return filepath.Join(c.config.WorkDir, "register_cli.py")
	}
	return filepath.Join(c.config.WorkDir, "protocol_register_cli.py")
}

func registrationProxyScope(accountType string) string {
	if accountType == "web" {
		return "grok_web"
	}
	return "grok_build"
}

func (c *Controller) resolveProxyGroupLocked(ctx context.Context, expectedScope string) error {
	value, err := readJSONMap(c.config.ConfigPath)
	if err != nil {
		return err
	}
	rawID := strings.TrimSpace(stringValue(value["proxy_group_id"], ""))
	if rawID == "" {
		delete(value, "proxy_pool")
		return writeJSONAtomic(c.config.ConfigPath, value, 0o600)
	}
	groupID, err := strconv.ParseUint(rawID, 10, 64)
	if err != nil || groupID == 0 {
		return fmt.Errorf("%w: 代理组 ID 无效", ErrInvalidInput)
	}
	if c.config.ResolveProxyGroup == nil {
		return fmt.Errorf("%w: 代理组解析器未配置", ErrInvalidInput)
	}
	proxies, err := c.config.ResolveProxyGroup(ctx, groupID, expectedScope)
	if err != nil {
		return fmt.Errorf("解析注册代理组: %w", err)
	}
	if len(proxies) == 0 {
		return fmt.Errorf("%w: 代理组没有可用节点", ErrInvalidInput)
	}
	value["proxy_pool"] = proxies
	return writeJSONAtomic(c.config.ConfigPath, value, 0o600)
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
func (c *Controller) browserStatePath() string {
	return filepath.Join(c.dataPath(), "browser_state.json")
}
func (c *Controller) workerStatePath(engine string) string {
	if engine == registrationEngineBrowser {
		return c.browserStatePath()
	}
	return c.protocolStatePath()
}
func (c *Controller) protocolLedgerPath() string {
	return filepath.Join(c.dataPath(), "protocol_accounts.jsonl")
}
func (c *Controller) browserLedgerPath() string {
	return filepath.Join(c.dataPath(), "accounts_cli.txt")
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
	solver := strings.ToLower(strings.TrimSpace(stringValue(value["clearance_provider"], stringValue(value["captcha_solver"], "docker"))))
	if solver == "docker" {
		solver = "local"
	}
	if solver != "yescaptcha" {
		solver = "local"
	}
	endpoint := strings.TrimSpace(stringValue(value["captcha_endpoint"], ""))
	if endpoint == "" {
		endpoint = strings.TrimSpace(stringValue(value["local_captcha_endpoint"], ""))
	}
	return WorkerSettings{
		Engine:                 normalizedRegistrationEngine(stringValue(value["engine"], registrationEngineProtocol)),
		EmailSources:           emailSourcesView(value),
		EmailProvider:          stringValue(value["email_provider"], "tempmail_lol"),
		EmailProviderFallbacks: stringSlice(value["email_provider_fallbacks"]),
		TempmailLolAPIBase:     stringValue(value["tempmail_lol_api_base"], "https://api.tempmail.lol/v2"),
		TempmailLolDomain:      stringValue(value["tempmail_lol_domain"], ""),
		TempmailLolPrefix:      stringValue(value["tempmail_lol_prefix"], ""),
		Proxy:                  stringValue(value["proxy"], ""), CPABaseURL: stringValue(value["cpa_base_url"], "https://cli-chat-proxy.grok.com/v1"),
		ProxyGroupID: stringValue(value["proxy_group_id"], ""),
		CPAProxy:     stringValue(value["cpa_proxy"], ""), CPAHeadless: boolValue(value["cpa_headless"], false),
		CPAProbeChat: boolValue(value["cpa_probe_chat"], true), CPACloseBrowserAfterAuth: boolValue(value["cpa_close_browser_after_auth"], true),
		CaptchaSolver: solver, CaptchaEndpoint: endpoint,
		YydsAPIKeyConfigured:    strings.TrimSpace(stringValue(value["yyds_api_key"], "")) != "",
		YydsJWTConfigured:       strings.TrimSpace(stringValue(value["yyds_jwt"], "")) != "",
		YesCaptchaKeyConfigured: strings.TrimSpace(stringValue(value["yescaptcha_api_key"], "")) != "",
	}
}

func applySettingsPatch(value map[string]any, patch WorkerSettingsPatch) error {
	if patch.Engine != nil {
		engine, err := normalizeRegistrationEngine(*patch.Engine)
		if err != nil {
			return fmt.Errorf("%w: 不支持的注册引擎", ErrInvalidInput)
		}
		value["engine"] = engine
	}
	if patch.CaptchaSolver != nil {
		solver := strings.ToLower(strings.TrimSpace(*patch.CaptchaSolver))
		if !slices.Contains([]string{"local", "yescaptcha"}, solver) {
			return fmt.Errorf("%w: 不支持的清障方式", ErrInvalidInput)
		}
		value["captcha_solver"] = solver
		if solver == "local" {
			value["clearance_provider"] = "docker"
		} else {
			value["clearance_provider"] = solver
		}
	}
	if patch.ProxyGroupID != nil {
		groupID := strings.TrimSpace(*patch.ProxyGroupID)
		if groupID != "" {
			if _, err := strconv.ParseUint(groupID, 10, 64); err != nil {
				return fmt.Errorf("%w: 代理组 ID 无效", ErrInvalidInput)
			}
		}
		value["proxy_group_id"] = groupID
	}
	if patch.CaptchaEndpoint != nil {
		endpoint, err := validateCaptchaEndpoint(*patch.CaptchaEndpoint)
		if err != nil {
			return fmt.Errorf("%w: 清障 endpoint 无效", ErrInvalidInput)
		}
		value["captcha_endpoint"] = endpoint
		value["clearance_endpoint"] = endpoint
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
	return slices.Contains([]string{
		"cloudmail_gen", "cloudflare_temp_email", "tempmail_lol", "moemail", "inbucket",
		"duckmail", "gptmail", "donemail", "yyds_mail", "yyds", "ddg_mail", "outlook_token",
	}, strings.TrimSpace(provider))
}

func isProtocolEmailProvider(provider string) bool {
	return slices.Contains([]string{"tempmail_lol", "yyds", "yyds_mail"}, strings.TrimSpace(provider))
}

func defaultEmailAPIBase(provider string) string {
	switch provider {
	case "tempmail_lol":
		return "https://api.tempmail.lol"
	case "yyds", "yyds_mail":
		return "https://maliapi.215.im/v1"
	case "gptmail":
		return "https://mail.chatgpt.org.uk"
	default:
		return ""
	}
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

	providers := append([]string{stringValue(value["email_provider"], "tempmail_lol")}, stringSlice(value["email_provider_fallbacks"])...)
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
		sources = append(sources, storedEmailSource{ID: "source-1", Type: "tempmail_lol", Enabled: true, APIBase: defaultEmailAPIBase("tempmail_lol")})
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
		options := make(map[string]any, len(source.Options))
		optionConfigured := make(map[string]bool, len(source.Options))
		for key, item := range source.Options {
			if item == nil || strings.TrimSpace(fmt.Sprint(item)) == "" {
				continue
			}
			if isSecretEmailOptionKey(key) {
				options[key] = ""
			} else {
				options[key] = item
			}
			optionConfigured[key] = true
		}
		sources = append(sources, EmailSourceSettings{
			ID: source.ID, Type: source.Type, Enabled: source.Enabled, APIBase: apiBase,
			Domain: source.Domain, Prefix: source.Prefix,
			APIKeyConfigured: strings.TrimSpace(source.APIKey) != "",
			JWTConfigured:    strings.TrimSpace(source.JWT) != "",
			Options:          options, OptionConfigured: optionConfigured,
		})
	}
	return sources
}

func isSecretEmailOptionKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, marker := range []string{"password", "token", "secret", "api_key", "apikey", "jwt", "refresh", "client_id", "mailboxes"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
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
		if apiBase == "" {
			if configured, ok := source.Options["api_base"].(string); ok {
				apiBase = strings.TrimSpace(configured)
			}
		}
		normalizedBase := strings.TrimRight(apiBase, "/")
		var err error
		if normalizedBase != "" {
			normalizedBase, err = validateHTTPURL(apiBase)
		}
		if (err != nil || normalizedBase == "") && !emailProviderAllowsEmptyAPIBase(source.Type) {
			return fmt.Errorf("%w: email source API base is invalid", ErrInvalidInput)
		}
		if len(source.Domain) > 2048 || len(source.Prefix) > 128 {
			return fmt.Errorf("%w: email source configuration is too long", ErrInvalidInput)
		}
		current := existing[source.ID]
		options := make(map[string]any, len(source.Options))
		if current.Type == source.Type {
			for key, item := range current.Options {
				options[key] = item
			}
		}
		for key, item := range source.Options {
			key = strings.TrimSpace(key)
			if key == "" || len(key) > 80 {
				return fmt.Errorf("%w: email source option key is invalid", ErrInvalidInput)
			}
			if len(fmt.Sprint(item)) > 16384 {
				return fmt.Errorf("%w: email source option is too long", ErrInvalidInput)
			}
			if isSecretEmailOptionKey(key) && strings.TrimSpace(fmt.Sprint(item)) == "" {
				if previous, ok := current.Options[key]; ok {
					options[key] = previous
					continue
				}
			}
			options[key] = item
		}
		if source.Type == "outlook_token" && options["mailboxes"] == nil {
			options["mailboxes"] = ""
		}
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
		if normalizedBase != "" {
			options["api_base"] = normalizedBase
		}
		if strings.TrimSpace(source.Domain) != "" {
			options["domain"] = strings.TrimSpace(source.Domain)
		}
		if secret != "" {
			options["api_key"] = secret
		}
		engine, _ := normalizeRegistrationEngine(stringValue(value["engine"], registrationEngineProtocol))
		if engine == registrationEngineProtocol && !isProtocolEmailProvider(source.Type) && source.Enabled {
			return fmt.Errorf("%w: %s requires the browser registration engine", ErrInvalidInput, source.Type)
		}
		if engine == registrationEngineBrowser && source.Enabled {
			if missing := missingEmailProviderOptions(source.Type, options); len(missing) > 0 {
				return fmt.Errorf("%w: %s requires %s", ErrInvalidInput, source.Type, strings.Join(missing, ", "))
			}
		}
		item := storedEmailSource{
			ID: source.ID, Type: source.Type, Enabled: source.Enabled, APIBase: normalizedBase,
			APIKey: secret, JWT: jwt, Domain: strings.TrimSpace(source.Domain), Prefix: strings.TrimSpace(source.Prefix),
			Options: options,
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

func emailProviderAllowsEmptyAPIBase(provider string) bool {
	return slices.Contains([]string{"outlook_token", "duckmail", "gptmail", "tempmail_lol", "yyds", "yyds_mail"}, provider)
}

func missingEmailProviderOptions(provider string, options map[string]any) []string {
	required := map[string][]string{
		"cloudmail_gen": {"api_base", "admin_email", "admin_password", "domain"},
		"cloudflare_temp_email": {"api_base", "admin_password", "domain"},
		"moemail": {"api_base", "api_key"},
		"inbucket": {"api_base", "domain"},
		"duckmail": {"api_key"},
		"donemail": {"api_base", "admin_key"},
		"ddg_mail": {"ddg_token", "api_base"},
		"outlook_token": {"mailboxes"},
	}
	missing := make([]string, 0)
	for _, key := range required[provider] {
		if value, ok := options[key]; !ok || strings.TrimSpace(fmt.Sprint(value)) == "" {
			missing = append(missing, key)
		}
	}
	return missing
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
	prefix := workerCommandPrefix(command)
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

func browserWorkerArguments(command []string, script string, arguments ...string) []string {
	prefix := workerCommandPrefix(command)
	for _, value := range prefix {
		if filepath.Base(value) == filepath.Base(script) {
			return append(prefix, arguments...)
		}
	}
	if isPythonCommand(command[0]) {
		pythonFlags := make([]string, 0, len(prefix)+2)
		for _, value := range prefix {
			if strings.HasSuffix(strings.ToLower(value), ".py") {
				continue
			}
			pythonFlags = append(pythonFlags, value)
		}
		if !slices.Contains(pythonFlags, "-u") {
			pythonFlags = append(pythonFlags, "-u")
		}
		return append(append(pythonFlags, script), arguments...)
	}
	return append(append(prefix, "--browser-worker"), arguments...)
}

func workerCommandPrefix(command []string) []string {
	prefix := make([]string, 0, max(0, len(command)-1))
	for _, value := range command[1:] {
		if value == "--protocol-worker" || value == "--browser-worker" {
			continue
		}
		prefix = append(prefix, value)
	}
	return prefix
}

func normalizeRegistrationEngine(value string) (string, error) {
	engine := strings.ToLower(strings.TrimSpace(value))
	if engine == "" {
		engine = registrationEngineProtocol
	}
	if engine != registrationEngineProtocol && engine != registrationEngineBrowser {
		return "", ErrInvalidInput
	}
	return engine, nil
}

func normalizedRegistrationEngine(value string) string {
	engine, err := normalizeRegistrationEngine(value)
	if err != nil {
		return registrationEngineProtocol
	}
	return engine
}

func isPythonCommand(command string) bool {
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(command, `\`, "/")))
	base = strings.TrimSuffix(base, ".exe")
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
		if values, ok := value.([]string); ok {
			result := make([]string, len(values))
			copy(result, values)
			return result
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
