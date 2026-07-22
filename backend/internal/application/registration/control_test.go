package registration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRegistrationHelperProcess(t *testing.T) {
	if os.Getenv("GO_REGISTRATION_HELPER") != "1" {
		return
	}
	if slices.Contains(os.Args, "--help") {
		fmt.Println("synthetic registration worker help")
		return
	}
	if path := os.Getenv("GO_REGISTRATION_HELPER_ARGS_FILE"); path != "" {
		_ = os.WriteFile(path, []byte(strings.Join(os.Args[1:], "\n")), 0o600)
	}
	if slices.Contains(os.Args, "--preflight") {
		fmt.Println("[preflight] OK synthetic protocol dependencies")
		return
	}
	if slices.Contains(os.Args, "--protocol-worker") {
		stateDir := ""
		for index, argument := range os.Args {
			if argument == "--state-dir" && index+1 < len(os.Args) {
				stateDir = os.Args[index+1]
				break
			}
		}
		if stateDir != "" {
			_ = os.MkdirAll(stateDir, 0o700)
			state, _ := json.Marshal(map[string]any{"done": 1, "target": 1})
			_ = os.WriteFile(filepath.Join(stateDir, "state.json"), state, 0o600)
			_ = os.WriteFile(filepath.Join(stateDir, "protocol_accounts.jsonl"), []byte("{\"email\":\"synthetic@example.invalid\"}\n"), 0o600)
		}
	}
	if slices.Contains(os.Args, "--browser-worker") {
		stateDir := ""
		for index, argument := range os.Args {
			if argument == "--state-dir" && index+1 < len(os.Args) {
				stateDir = os.Args[index+1]
				break
			}
		}
		if stateDir != "" {
			_ = os.MkdirAll(stateDir, 0o700)
			state, _ := json.Marshal(map[string]any{
				"done": 1, "target": 1, "attempted": 1, "ok": 1, "failed": 0, "registered": 1,
			})
			_ = os.WriteFile(filepath.Join(stateDir, "browser_state.json"), state, 0o600)
		}
	}
	for index, argument := range os.Args {
		if argument == "--accounts-file" && index+1 < len(os.Args) {
			_ = os.WriteFile(os.Args[index+1], []byte("synthetic@example.invalid\n"), 0o600)
		}
	}
	if value, err := strconv.Atoi(os.Getenv("GO_REGISTRATION_HELPER_SLEEP_MS")); err == nil && value > 0 {
		time.Sleep(time.Duration(value) * time.Millisecond)
	}
	fmt.Println("=== 完成: 注册成功 1, 注册失败 0, CPA成功 1, CPA失败 0, CPA跳过 0 ===")
}

func TestControllerStartsPersistsStateAndReturnsNewestLogs(t *testing.T) {
	controller := newControllerTest(t, 0)
	status, err := controller.Start(context.Background(), StartInput{Count: 1, Threads: 1, Fast: true})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if !status.Running || status.PID == 0 {
		t.Fatalf("unexpected running status: %+v", status)
	}

	status = waitForStopped(t, controller)
	if status.ExitCode == nil || *status.ExitCode != 0 || status.LastError != nil {
		exitCode := "<nil>"
		if status.ExitCode != nil {
			exitCode = strconv.Itoa(*status.ExitCode)
		}
		errorCode := "<nil>"
		if status.LastError != nil {
			errorCode = status.LastError.Code
		}
		t.Fatalf("unexpected completed status: %+v exit=%s error=%s logs=%+v", status, exitCode, errorCode, mustLogs(t, controller))
	}
	if status.Progress.AccountCount != 1 || status.Progress.Done != 1 || status.Progress.Percent == nil || *status.Progress.Percent != 100 {
		t.Fatalf("unexpected progress: %+v", status.Progress)
	}
	logs, err := controller.Logs(20)
	if err != nil || len(logs.Items) < 2 {
		t.Fatalf("Logs() = %+v, error = %v", logs, err)
	}
	if logs.Items[0].ID <= logs.Items[1].ID {
		t.Fatalf("logs are not newest-first: %+v", logs.Items)
	}

	settings, err := controller.Settings()
	if err != nil || settings.EmailProvider == "" {
		t.Fatalf("Settings() = %+v, error = %v", settings, err)
	}
	proxy := "http://127.0.0.1:8080"
	updated, err := controller.UpdateSettings(WorkerSettingsPatch{Proxy: &proxy})
	if err != nil || updated.Proxy != proxy {
		t.Fatalf("UpdateSettings() = %+v, error = %v", updated, err)
	}
	configData, err := os.ReadFile(controller.config.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(configData, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["cpa_remote_import_enabled"] != false || raw["grok2api_auto_add_remote"] != false {
		t.Fatalf("unsafe integration settings were not forced off: %#v", raw)
	}
	if raw["spool_dir"] != filepath.Join(controller.config.SpoolPath, "incoming") {
		t.Fatalf("protocol spool directory was not forced to the private importer queue: %#v", raw["spool_dir"])
	}
}

func TestControllerResolvesSystemProxyForWorker(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	proxyURL := "http://" + listener.Addr().String()
	t.Setenv("HTTPS_PROXY", proxyURL)
	argsPath := filepath.Join(t.TempDir(), "worker-args.txt")
	t.Setenv("GO_REGISTRATION_HELPER_ARGS_FILE", argsPath)

	controller := newControllerTest(t, 0)
	mode := "system"
	if _, err := controller.UpdateSettings(WorkerSettingsPatch{Proxy: &mode}); err != nil {
		t.Fatal(err)
	}
	preflight := controller.Preflight(context.Background())
	if !preflight.OK {
		t.Fatalf("system proxy preflight = %+v", preflight)
	}
	if _, err := controller.Start(context.Background(), StartInput{Count: 1, Threads: 1}); err != nil {
		t.Fatal(err)
	}
	waitForStopped(t, controller)
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	arguments := strings.Split(string(data), "\n")
	proxyIndex := slices.Index(arguments, "--proxy")
	if proxyIndex < 0 || proxyIndex+1 >= len(arguments) || arguments[proxyIndex+1] != proxyURL {
		t.Fatalf("worker arguments = %q, want resolved proxy %q", arguments, proxyURL)
	}
}

func TestNormalizeSystemProxySetting(t *testing.T) {
	tests := map[string]string{
		"":               "",
		"127.0.0.1:7897": "http://127.0.0.1:7897",
		"https=http-proxy:8443;http=http-proxy:8080": "http://http-proxy:8443",
		"socks=127.0.0.1:1080":                       "socks5://127.0.0.1:1080",
		"socks5://127.0.0.1:1080":                    "socks5://127.0.0.1:1080",
	}
	for input, want := range tests {
		if got := normalizeSystemProxySetting(input); got != want {
			t.Errorf("normalizeSystemProxySetting(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRegistrationSpecificProxyPrecedesProcessProxy(t *testing.T) {
	t.Setenv("REGISTRATION_HTTPS_PROXY", "http://registration-proxy:8443")
	t.Setenv("HTTPS_PROXY", "http://process-proxy:8080")
	if got := systemProxyURL(); got != "http://registration-proxy:8443" {
		t.Fatalf("systemProxyURL() = %q", got)
	}
}

func TestExplicitProxyDoesNotFallBackToSystem(t *testing.T) {
	lookupCalled := false
	_, ok, _ := resolveRegistrationProxyWithSystem("http://127.0.0.1:1", func() string {
		lookupCalled = true
		return "http://127.0.0.1:7897"
	})
	if ok || lookupCalled {
		t.Fatalf("explicit proxy result ok=%v lookupCalled=%v", ok, lookupCalled)
	}
}

func TestDeploymentEnvironmentOverridesWorkerNetworkSettings(t *testing.T) {
	t.Setenv("REGISTRATION_CAPTCHA_ENDPOINT", "http://grok-turnstile-solver:5072")
	t.Setenv("REGISTRATION_PROXY", "")
	controller := newControllerTest(t, 0)
	value := map[string]any{
		"captcha_endpoint": "docker://legacy-solver:5072",
		"proxy":            "http://127.0.0.1:7890",
	}

	controller.forceSafeWorkerSettings(value)

	if value["captcha_endpoint"] != "http://grok-turnstile-solver:5072" {
		t.Fatalf("captcha endpoint = %#v", value["captcha_endpoint"])
	}
	if value["proxy"] != "" {
		t.Fatalf("proxy = %#v, want empty deployment override", value["proxy"])
	}
}

func TestLegacyCaptchaEndpointMigratesToComposeService(t *testing.T) {
	previous, present := os.LookupEnv("REGISTRATION_CAPTCHA_ENDPOINT")
	if err := os.Unsetenv("REGISTRATION_CAPTCHA_ENDPOINT"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv("REGISTRATION_CAPTCHA_ENDPOINT", previous)
		} else {
			_ = os.Unsetenv("REGISTRATION_CAPTCHA_ENDPOINT")
		}
	})

	controller := newControllerTest(t, 0)
	value := map[string]any{"captcha_endpoint": "docker://grokcli-2api:5072"}
	controller.forceSafeWorkerSettings(value)

	if value["captcha_endpoint"] != defaultCaptchaEndpoint || value["clearance_endpoint"] != defaultCaptchaEndpoint {
		t.Fatalf("legacy captcha endpoint was not migrated: %#v", value)
	}
}

func TestEmailSourcesMigratePersistAndDriveProviderOrder(t *testing.T) {
	value := map[string]any{
		"email_provider":           "yyds",
		"email_provider_fallbacks": []string{},
		"yyds_api_key":             "existing-secret",
	}

	view := settingsView(value)
	if len(view.EmailSources) != 1 || view.EmailSources[0].Type != "yyds" || !view.EmailSources[0].APIKeyConfigured {
		t.Fatalf("legacy source migration = %+v", view.EmailSources)
	}
	if view.EmailSources[0].APIKey != "" {
		t.Fatal("settings view leaked an email source secret")
	}

	patch := []EmailSourceSettings{
		{ID: view.EmailSources[0].ID, Type: "yyds", Enabled: true, APIBase: "https://maliapi.215.im/v1"},
		{ID: "source-2", Type: "tempmail_lol", Enabled: true, APIBase: "https://api.tempmail.lol", Prefix: "xai"},
	}
	if err := applyEmailSourcesPatch(value, patch); err != nil {
		t.Fatalf("applyEmailSourcesPatch() error = %v", err)
	}
	if value["email_provider"] != "yyds" {
		t.Fatalf("primary provider = %#v", value["email_provider"])
	}
	fallbacks := stringSlice(value["email_provider_fallbacks"])
	if len(fallbacks) != 1 || fallbacks[0] != "tempmail_lol" {
		t.Fatalf("fallback providers = %#v", fallbacks)
	}
	if value["yyds_api_key"] != "existing-secret" {
		t.Fatal("blank secret patch did not preserve the configured YYDS key")
	}

	patch[0].Enabled = false
	if err := applyEmailSourcesPatch(value, patch); err != nil {
		t.Fatalf("apply disabled source patch: %v", err)
	}
	if value["email_provider"] != "tempmail_lol" || len(stringSlice(value["email_provider_fallbacks"])) != 0 {
		t.Fatalf("enabled provider order was not synchronized: %#v", value)
	}
}

func TestUpdateSettingsKeepsEmptyFallbacksAsJSONArray(t *testing.T) {
	controller := newControllerTest(t, 0)
	empty := []string{}
	settings, err := controller.UpdateSettings(WorkerSettingsPatch{EmailProviderFallbacks: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if settings.EmailProviderFallbacks == nil {
		t.Fatal("emailProviderFallbacks is nil; JSON would contain null")
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"emailProviderFallbacks":[]`) {
		t.Fatalf("settings response does not preserve an empty JSON array: %s", encoded)
	}
}

func TestEmailSourcesRejectDuplicateTypesAndAllDisabled(t *testing.T) {
	value := map[string]any{"email_provider": "yyds"}
	duplicate := []EmailSourceSettings{
		{ID: "one", Type: "yyds", Enabled: true, APIBase: "https://maliapi.215.im/v1"},
		{ID: "two", Type: "yyds", Enabled: true, APIBase: "https://maliapi.215.im/v1"},
	}
	if err := applyEmailSourcesPatch(value, duplicate); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate type error = %v", err)
	}
	allDisabled := []EmailSourceSettings{{ID: "one", Type: "tempmail_lol", Enabled: false, APIBase: "https://api.tempmail.lol"}}
	if err := applyEmailSourcesPatch(value, allDisabled); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("all-disabled error = %v", err)
	}
}

func TestEmailSourceOptionsPreserveSecretsAndValidateBrowserProviders(t *testing.T) {
	value := map[string]any{
		"engine": "browser",
		"email_sources": []any{map[string]any{
			"id": "outlook", "type": "outlook_token", "enabled": true,
			"options": map[string]any{"mailboxes": "account----password----client----refresh"},
		}},
	}
	patch := []EmailSourceSettings{{
		ID: "outlook", Type: "outlook_token", Enabled: true,
		Options: map[string]any{"mailboxes": ""},
	}}
	if err := applyEmailSourcesPatch(value, patch); err != nil {
		t.Fatalf("redacted option patch should preserve the existing secret: %v", err)
	}
	stored := readStoredEmailSources(value)
	if len(stored) != 1 || stored[0].Options["mailboxes"] != "account----password----client----refresh" {
		t.Fatalf("stored secret option was lost: %#v", stored)
	}

	missing := []EmailSourceSettings{{ID: "cloud", Type: "cloudmail_gen", Enabled: true, APIBase: "https://mail.example.test"}}
	if err := applyEmailSourcesPatch(map[string]any{"engine": "browser"}, missing); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("incomplete browser provider should be rejected: %v", err)
	}
}

func TestResolveProxyGroupPersistsWorkerProxyPool(t *testing.T) {
	controller := newControllerTest(t, 0)
	controller.config.ResolveProxyGroup = func(_ context.Context, id uint64, _ string) ([]string, error) {
		if id != 7 {
			t.Fatalf("resolved group id = %d", id)
		}
		return []string{"http://proxy-a:8080", "socks5://proxy-b:1080"}, nil
	}
	if err := os.MkdirAll(filepath.Dir(controller.config.ConfigPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(controller.config.ConfigPath, []byte(`{"proxy_group_id":"7","proxy":"legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := controller.resolveProxyGroupLocked(context.Background(), "grok_build"); err != nil {
		t.Fatal(err)
	}
	value, err := readJSONMap(controller.config.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	pool := stringSlice(value["proxy_pool"])
	if len(pool) != 2 || pool[0] != "http://proxy-a:8080" || value["proxy"] != "legacy" {
		t.Fatalf("resolved config = %#v", value)
	}
	value["proxy_group_id"] = ""
	if err := writeJSONAtomic(controller.config.ConfigPath, value, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := controller.resolveProxyGroupLocked(context.Background(), "grok_build"); err != nil {
		t.Fatal(err)
	}
	value, err = readJSONMap(controller.config.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := value["proxy_pool"]; exists {
		t.Fatalf("proxy pool was not removed: %#v", value)
	}
}

func mustLogs(t *testing.T, controller *Controller) LogResult {
	t.Helper()
	logs, err := controller.Logs(20)
	if err != nil {
		t.Fatal(err)
	}
	return logs
}

func TestControllerStopsWorkerProcessTree(t *testing.T) {
	controller := newControllerTest(t, 30_000)
	status, err := controller.Start(context.Background(), StartInput{Count: 0, Threads: 1, Fast: true})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if !status.Running {
		t.Fatal("worker exited before stop test")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, err = controller.Stop(stopCtx)
	if err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if status.Running || status.LastError == nil || status.LastError.Code != "registrationStopped" {
		t.Fatalf("unexpected stopped status: %+v", status)
	}
}

func TestControllerProtocolModeUsesConfiguredWorkerDispatcher(t *testing.T) {
	controller := newControllerTest(t, 0)
	argumentsPath := filepath.Join(t.TempDir(), "arguments.txt")
	t.Setenv("GO_REGISTRATION_HELPER_ARGS_FILE", argumentsPath)
	if err := os.WriteFile(filepath.Join(controller.config.WorkDir, "protocol_register_cli.py"), []byte("# synthetic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{
		"engine":                       "protocol",
		"email_provider":               "yyds",
		"yyds_api_key":                 "synthetic",
		"captcha_solver":               "yescaptcha",
		"yescaptcha_api_key":           "synthetic",
		"cpa_base_url":                 "https://cli-chat-proxy.grok.com/v1",
		"cpa_probe_chat":               true,
		"cpa_close_browser_after_auth": true,
	}
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(controller.config.ConfigPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(controller.config.ConfigPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := controller.Start(context.Background(), StartInput{Count: 1, Threads: 1, Fast: true, AccountType: "web"}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	status := waitForStopped(t, controller)
	if status.ExitCode == nil || *status.ExitCode != 0 {
		t.Fatalf("unexpected protocol worker status: %+v", status)
	}
	if status.Progress.Done != 1 || status.Progress.Total == nil || *status.Progress.Total != 1 || status.Progress.AccountCount != 1 || status.Progress.Percent == nil || *status.Progress.Percent != 100 {
		t.Fatalf("unexpected protocol progress: %+v", status.Progress)
	}
	arguments, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	values := strings.Split(string(arguments), "\n")
	if !slices.Contains(values, "--protocol-worker") {
		t.Fatalf("protocol dispatcher argument missing: %q", values)
	}
	accountTypeIndex := slices.Index(values, "--account-type")
	if accountTypeIndex < 0 || accountTypeIndex+1 >= len(values) || values[accountTypeIndex+1] != "web" {
		t.Fatalf("web account type argument missing: %q", values)
	}
	if slices.Contains(values, "-u") || slices.Contains(values, filepath.Join(controller.config.WorkDir, "protocol_register_cli.py")) {
		t.Fatalf("protocol script leaked through the configured worker wrapper: %q", values)
	}
}

func TestControllerBrowserModeUsesSelectedWebWorkerAndBrowserState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/ip" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"ip":"203.0.113.10"}`))
			return
		}
		if request.URL.Path == "/signup" {
			_, _ = writer.Write([]byte("<html><body>sign up</body></html>"))
			return
		}
		writer.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	controller := newControllerTest(t, 0)
	prepareBrowserControllerTest(t, controller)
	argumentsPath := filepath.Join(t.TempDir(), "browser-arguments.txt")
	t.Setenv("GO_REGISTRATION_HELPER_ARGS_FILE", argumentsPath)
	t.Setenv("REGISTRATION_BROWSER_MODE", "headless")
	t.Setenv("REGISTRATION_PREFLIGHT_EGRESS_URL", server.URL+"/ip")

	engine := registrationEngineBrowser
	sources := []EmailSourceSettings{{ID: "mail", Type: "tempmail_lol", Enabled: true, APIBase: server.URL}}
	settings, err := controller.UpdateSettings(WorkerSettingsPatch{Engine: &engine, EmailSources: &sources})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Engine != registrationEngineBrowser {
		t.Fatalf("browser engine was not persisted: %+v", settings)
	}
	config, err := readJSONMap(controller.config.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	config["signup_url"] = server.URL + "/signup"
	if err := writeJSONAtomic(controller.config.ConfigPath, config, 0o600); err != nil {
		t.Fatal(err)
	}

	preflight := controller.Preflight(context.Background())
	if !preflight.OK {
		t.Fatalf("browser preflight = %+v", preflight)
	}
	for _, check := range preflight.Checks {
		if strings.HasPrefix(check.Name, "captcha") || check.Name == "yescaptcha" {
			t.Fatalf("browser preflight depended on token solver: %+v", check)
		}
	}
	if check, ok := preflightCheck(preflight.Checks, "egressIP"); !ok || !check.OK || !strings.Contains(check.Detail, "203.0.113.10") {
		t.Fatalf("browser preflight did not record its egress IP: %+v", check)
	}

	status, err := controller.Start(context.Background(), StartInput{Count: 1, Threads: 3, AccountType: "web", AutoNSFW: true})
	if err != nil {
		t.Fatal(err)
	}
	if !status.Running {
		t.Fatalf("browser worker did not start: %+v", status)
	}
	status = waitForStopped(t, controller)
	if status.ExitCode == nil || *status.ExitCode != 0 || status.Progress.Done != 1 || status.Progress.Succeeded != 1 || status.Progress.AccountCount != 1 {
		t.Fatalf("browser status = %+v", status)
	}

	data, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	arguments := strings.Split(string(data), "\n")
	for _, required := range []string{"--browser-worker", "--auto-nsfw", "--state-dir", "--log-file", "--accounts-file"} {
		if !slices.Contains(arguments, required) {
			t.Fatalf("browser argument %q missing: %q", required, arguments)
		}
	}
	accountTypeIndex := slices.Index(arguments, "--account-type")
	if accountTypeIndex < 0 || accountTypeIndex+1 >= len(arguments) || arguments[accountTypeIndex+1] != "web" {
		t.Fatalf("browser worker did not preserve the selected Web account type: %q", arguments)
	}
	if slices.Contains(arguments, "--protocol-worker") || slices.Contains(arguments, "--inline-mint") {
		t.Fatalf("Build-only arguments leaked into the Browser Web worker: %q", arguments)
	}
}

func TestControllerBrowserBuildUsesInlineMint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/ip" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"ip":"203.0.113.10"}`))
			return
		}
		if request.URL.Path == "/signup" {
			_, _ = writer.Write([]byte("<html><body>sign up</body></html>"))
			return
		}
		writer.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	controller := newControllerTest(t, 0)
	prepareBrowserControllerTest(t, controller)
	argumentsPath := filepath.Join(t.TempDir(), "browser-build-arguments.txt")
	t.Setenv("GO_REGISTRATION_HELPER_ARGS_FILE", argumentsPath)
	t.Setenv("REGISTRATION_BROWSER_MODE", "headless")
	t.Setenv("REGISTRATION_PREFLIGHT_EGRESS_URL", server.URL+"/ip")

	engine := registrationEngineBrowser
	sources := []EmailSourceSettings{{ID: "mail", Type: "tempmail_lol", Enabled: true, APIBase: server.URL}}
	if _, err := controller.UpdateSettings(WorkerSettingsPatch{Engine: &engine, EmailSources: &sources}); err != nil {
		t.Fatal(err)
	}
	config, err := readJSONMap(controller.config.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	config["signup_url"] = server.URL + "/signup"
	if err := writeJSONAtomic(controller.config.ConfigPath, config, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := controller.Start(context.Background(), StartInput{Count: 1, Threads: 1, AccountType: "build"}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	status := waitForStopped(t, controller)
	if status.ExitCode == nil || *status.ExitCode != 0 {
		t.Fatalf("browser Build status = %+v", status)
	}
	data, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	arguments := strings.Split(string(data), "\n")
	if !slices.Contains(arguments, "--inline-mint") || slices.Contains(arguments, "--auto-nsfw") {
		t.Fatalf("unexpected Browser Build arguments: %q", arguments)
	}
	accountTypeIndex := slices.Index(arguments, "--account-type")
	if accountTypeIndex < 0 || accountTypeIndex+1 >= len(arguments) || arguments[accountTypeIndex+1] != "build" {
		t.Fatalf("browser worker did not preserve the selected Build account type: %q", arguments)
	}
}

func TestBrowserWorkerArgumentsSupportConfiguredCommandShapes(t *testing.T) {
	script := filepath.Join("registration", "register_cli.py")
	protocolScript := filepath.Join("registration", "protocol_register_cli.py")
	tests := []struct {
		name    string
		command []string
		want    []string
	}{
		{name: "wrapper", command: []string{"grok2api-registration"}, want: []string{"--browser-worker", "--help"}},
		{name: "wrapper-with-protocol-dispatcher", command: []string{"grok2api-registration", "--protocol-worker"}, want: []string{"--browser-worker", "--help"}},
		{name: "browser-script", command: []string{"python", "-u", script}, want: []string{"-u", script, "--help"}},
		{name: "protocol-script", command: []string{"python", "-u", protocolScript}, want: []string{"-u", script, "--help"}},
		{name: "python-only", command: []string{"python"}, want: []string{"-u", script, "--help"}},
		{name: "windows-python-path", command: []string{`C:\Python313\python.exe`, "-u", protocolScript}, want: []string{"-u", script, "--help"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := browserWorkerArguments(test.command, script, "--help")
			if !slices.Equal(got, test.want) {
				t.Fatalf("browserWorkerArguments(%q) = %q, want %q", test.command, got, test.want)
			}
		})
	}
}

func TestBrowserPreflightSupportsHTTPProxyAuthAndRejectsAuthenticatedSOCKS(t *testing.T) {
	ok, detail := browserProxyAuthenticationReady("http://user:secret@proxy.example:8080")
	if !ok || !strings.Contains(detail, "auth extension") {
		t.Fatalf("authenticated browser proxy check = %v, %q", ok, detail)
	}
	ok, detail = browserProxyAuthenticationReady("socks5://user:secret@proxy.example:1080")
	if ok || !strings.Contains(detail, "local unauthenticated relay") {
		t.Fatalf("authenticated SOCKS proxy check = %v, %q", ok, detail)
	}
	if ok, _ := browserProxyAuthenticationReady("http://proxy.example:8080"); !ok {
		t.Fatal("unauthenticated browser proxy was rejected")
	}
}

func TestProbeBrowserRegistrationPageRejectsCloudflareChallenge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("cf-mitigated", "challenge")
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte("<title>Attention Required! | Cloudflare</title>"))
	}))
	defer server.Close()

	ok, detail := probeBrowserRegistrationPage(context.Background(), server.URL, "")
	if ok || !strings.Contains(detail, "403") {
		t.Fatalf("Cloudflare registration probe = %v, %q", ok, detail)
	}
}

func TestProbeEgressIPAcceptsJSONAndTraceResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/trace" {
			_, _ = writer.Write([]byte("fl=123\nip=2001:db8::10\nloc=ZZ\n"))
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ip":"203.0.113.20"}`))
	}))
	defer server.Close()

	for _, test := range []struct {
		path string
		ip   string
	}{{path: "/json", ip: "203.0.113.20"}, {path: "/trace", ip: "2001:db8::10"}} {
		ok, detail := probeEgressIP(context.Background(), server.URL+test.path, "")
		if !ok || !strings.Contains(detail, test.ip) {
			t.Fatalf("probeEgressIP(%q) = %v, %q", test.path, ok, detail)
		}
	}
}

func preflightCheck(checks []PreflightCheck, name string) (PreflightCheck, bool) {
	for _, check := range checks {
		if check.Name == name {
			return check, true
		}
	}
	return PreflightCheck{}, false
}

func TestParseWorkerPreflightChecksUsesStructuredBrowserOutput(t *testing.T) {
	output := []byte("browser warning\n{\"ok\":false,\"checks\":{\"grok_register_ttk\":{\"ok\":true,\"detail\":\"/app/grok_register_ttk.py\"},\"DrissionPage\":{\"ok\":false,\"detail\":\"module missing\"}}}\n")
	checks := parseWorkerPreflightChecks(output)
	if check := checks["grok_register_ttk"]; !check.OK || !strings.Contains(check.Detail, "grok_register_ttk.py") {
		t.Fatalf("grok_register_ttk check = %+v", check)
	}
	if check := checks["DrissionPage"]; check.OK || check.Detail != "module missing" {
		t.Fatalf("DrissionPage check = %+v", check)
	}
}

func TestWorkerPreflightFailureDetailIncludesConcreteFailures(t *testing.T) {
	detail := workerPreflightFailureDetail(map[string]workerPreflightCheck{
		"DrissionPage":     {OK: true, Detail: "importable"},
		"registrationPage": {OK: false, Detail: "Cloudflare challenge"},
		"chromium":         {OK: false, Detail: "not found"},
	})
	if !strings.Contains(detail, "chromium: not found") || !strings.Contains(detail, "registrationPage: Cloudflare challenge") {
		t.Fatalf("worker preflight detail = %q", detail)
	}
}

func TestWorkerEnvironmentInheritsAndOverridesBrowserConfig(t *testing.T) {
	t.Setenv("REGISTRATION_BROWSER_MODE", "xvfb")
	t.Setenv("REGISTRATION_BROWSER_PATH", "/usr/bin/chromium")
	controller := &Controller{config: Config{SpoolPath: t.TempDir()}}
	if value := environmentValue(controller.workerEnvironment(), "REGISTRATION_BROWSER_MODE"); value != "xvfb" {
		t.Fatalf("inherited browser mode = %q", value)
	}
	if value := environmentValue(controller.workerEnvironment(), "REGISTRATION_BROWSER_PATH"); value != "/usr/bin/chromium" {
		t.Fatalf("inherited browser path = %q", value)
	}

	controller.config.BrowserMode = "background"
	controller.config.BrowserPath = "/opt/chromium"
	if value := environmentValue(controller.workerEnvironment(), "REGISTRATION_BROWSER_MODE"); value != "background" {
		t.Fatalf("overridden browser mode = %q", value)
	}
	if value := environmentValue(controller.workerEnvironment(), "REGISTRATION_BROWSER_PATH"); value != "/opt/chromium" {
		t.Fatalf("overridden browser path = %q", value)
	}
}

func TestWorkerEnvironmentForBrowserEngineDefaultsToHeadless(t *testing.T) {
	controller := &Controller{config: Config{SpoolPath: t.TempDir()}}
	if value := environmentValue(controller.workerEnvironmentForEngine(registrationEngineBrowser), "REGISTRATION_BROWSER_MODE"); value != "headless" {
		t.Fatalf("default browser mode = %q", value)
	}
}

func TestResolveBrowserExecutableDoesNotFallbackToEdge(t *testing.T) {
	t.Setenv("REGISTRATION_BROWSER_PATH", "")
	t.Setenv("PATH", t.TempDir())
	if runtime.GOOS == "windows" {
		root := t.TempDir()
		edge := filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe")
		if err := os.MkdirAll(filepath.Dir(edge), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(edge, []byte{}, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PROGRAMFILES", root)
		t.Setenv("PROGRAMFILES(X86)", "")
		t.Setenv("LOCALAPPDATA", "")
	}
	if path, err := resolveBrowserExecutable(""); err == nil || path != "" {
		t.Fatalf("resolveBrowserExecutable unexpectedly selected %q, err=%v", path, err)
	}
}

func TestProtocolWorkerArgumentsSupportConfiguredCommandShapes(t *testing.T) {
	script := filepath.Join("registration", "protocol_register_cli.py")
	tests := []struct {
		name    string
		command []string
		want    []string
	}{
		{name: "wrapper", command: []string{"grok2api-registration"}, want: []string{"--protocol-worker", "--help"}},
		{name: "wrapper-with-browser-dispatcher", command: []string{"grok2api-registration", "--browser-worker"}, want: []string{"--protocol-worker", "--help"}},
		{name: "browser-script", command: []string{"python", "register_cli.py"}, want: []string{"register_cli.py", "--protocol-worker", "--help"}},
		{name: "protocol-script", command: []string{"python", "-u", script}, want: []string{"-u", script, "--help"}},
		{name: "python-only", command: []string{"python"}, want: []string{"-u", script, "--help"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := protocolWorkerArguments(test.command, script, "--help")
			if !slices.Equal(got, test.want) {
				t.Fatalf("protocolWorkerArguments(%q) = %q, want %q", test.command, got, test.want)
			}
		})
	}
}

func TestDisabledControllerReportsUnavailableWithoutTouchingWorkerFiles(t *testing.T) {
	controller := NewController(nil, Config{SpoolPath: filepath.Join(t.TempDir(), "spool")})
	status, err := controller.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.Configured || status.Running {
		t.Fatalf("disabled controller status = %+v", status)
	}
	preflight := controller.Preflight(context.Background())
	if preflight.OK || len(preflight.Checks) != 1 || preflight.Checks[0].Name != "enabled" {
		t.Fatalf("disabled controller preflight = %+v", preflight)
	}
	if _, err := controller.Settings(); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("disabled controller Settings() error = %v", err)
	}
}

func TestPreflightConfigKeepsArrayShapeWhenWorkerConfigCannotLoad(t *testing.T) {
	controller := newControllerTest(t, 0)
	if err := os.MkdirAll(controller.config.ConfigPath, 0o700); err != nil {
		t.Fatal(err)
	}

	preflight := controller.Preflight(context.Background())
	if preflight.OK {
		t.Fatalf("preflight unexpectedly passed: %+v", preflight)
	}
	if preflight.Config.EmailSources == nil {
		t.Fatal("preflight config emailSources is nil; JSON would contain null")
	}
	if preflight.Config.EmailProviderFallbacks == nil {
		t.Fatal("preflight config emailProviderFallbacks is nil; JSON would contain null")
	}
	encoded, err := json.Marshal(preflight)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"emailSources":[]`) || !strings.Contains(string(encoded), `"emailProviderFallbacks":[]`) {
		t.Fatalf("preflight config arrays are not stable: %s", encoded)
	}
}

func TestProtocolProgressReadsUsableOutcomeCounters(t *testing.T) {
	controller := newControllerTest(t, 0)
	state := map[string]any{
		"done": 2, "target": 3, "attempted": 4, "ok": 2, "failed": 2, "resumable": 1,
	}
	data, _ := json.Marshal(state)
	if err := os.MkdirAll(controller.dataPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(controller.protocolStatePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	target := 3
	progress := controller.protocolProgressLocked(persistedState{ProgressMode: "count", TargetCount: &target}, 10)
	if progress.Done != 2 || progress.Attempted != 4 || progress.Succeeded != 2 || progress.Failed != 2 || progress.Resumable != 1 {
		t.Fatalf("unexpected protocol progress: %+v", progress)
	}
	if progress.Percent == nil || *progress.Percent != float64(2)*100/3 {
		t.Fatalf("unexpected protocol percent: %+v", progress.Percent)
	}
}

func TestProtocolProgressMigratesLegacyDoneAttempts(t *testing.T) {
	controller := newControllerTest(t, 0)
	legacy := map[string]any{"done": 30, "target": 30, "ok": 0, "failed": 30}
	data, _ := json.Marshal(legacy)
	if err := os.MkdirAll(controller.dataPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(controller.protocolStatePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	target := 30
	progress := controller.protocolProgressLocked(persistedState{ProgressMode: "count", TargetCount: &target}, 45)
	if progress.Done != 0 || progress.Attempted != 30 || progress.Succeeded != 0 || progress.Failed != 30 {
		t.Fatalf("unexpected migrated progress: %+v", progress)
	}
	if progress.Percent == nil || *progress.Percent != 0 {
		t.Fatalf("unexpected migrated percent: %+v", progress.Percent)
	}
}

func TestBrowserProgressReadsBrowserState(t *testing.T) {
	controller := newControllerTest(t, 0)
	state := map[string]any{
		"done": 2, "target": 3, "attempted": 3, "ok": 2, "failed": 1, "registered": 3,
	}
	data, _ := json.Marshal(state)
	if err := os.MkdirAll(controller.dataPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(controller.browserStatePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	target := 3
	progress := controller.browserProgressLocked(persistedState{Engine: registrationEngineBrowser, ProgressMode: "count", TargetCount: &target}, 0)
	if progress.Done != 2 || progress.Attempted != 3 || progress.Succeeded != 2 || progress.Failed != 1 || progress.AccountCount != 3 {
		t.Fatalf("unexpected browser progress: %+v", progress)
	}
}

func TestRegistrationTimingMetrics(t *testing.T) {
	startedAt := time.Date(2026, time.July, 21, 10, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)

	durationMs, averagePerAccountMs := registrationTimingMetrics(
		persistedState{StartedAt: &startedAt, FinishedAt: &finishedAt},
		Progress{Succeeded: 2},
	)
	if durationMs == nil || *durationMs != int64((2*time.Minute)/time.Millisecond) {
		t.Fatalf("durationMs = %+v", durationMs)
	}
	if averagePerAccountMs == nil || *averagePerAccountMs != int64(time.Minute/time.Millisecond) {
		t.Fatalf("averagePerAccountMs = %+v", averagePerAccountMs)
	}

	durationMs, averagePerAccountMs = registrationTimingMetrics(
		persistedState{StartedAt: &startedAt, FinishedAt: &finishedAt},
		Progress{},
	)
	if durationMs == nil || *durationMs != int64((2*time.Minute)/time.Millisecond) {
		t.Fatalf("duration without completions = %+v", durationMs)
	}
	if averagePerAccountMs != nil {
		t.Fatalf("average without completions = %+v", averagePerAccountMs)
	}
}

func TestCountProtocolAccountsSupportsJSONLAndLegacyArray(t *testing.T) {
	root := t.TempDir()
	jsonl := filepath.Join(root, "protocol_accounts.jsonl")
	if err := os.WriteFile(jsonl, []byte("{\"email\":\"one@example.invalid\"}\ninvalid\n{\"email\":\"two@example.invalid\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := countProtocolAccounts(jsonl); got != 2 {
		t.Fatalf("JSONL count = %d, want 2", got)
	}
	if err := os.Remove(jsonl); err != nil {
		t.Fatal(err)
	}
	legacy, _ := json.Marshal([]map[string]string{{"email": "one"}, {"email": "two"}, {"email": "three"}})
	if err := os.WriteFile(filepath.Join(root, "protocol_accounts.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := countProtocolAccounts(jsonl); got != 3 {
		t.Fatalf("legacy count = %d, want 3", got)
	}
}

func TestClassifyFailureReportsPartialProtocolCompletion(t *testing.T) {
	controller := newControllerTest(t, 0)
	failure := controller.classifyFailureLocked(3, false)
	if failure == nil || failure.Code != "registrationPartial" {
		t.Fatalf("partial failure = %+v", failure)
	}
}

func TestClassifyFailureDoesNotTreatEveryCPAExitAsChatProbeFailure(t *testing.T) {
	controller := newControllerTest(t, 0)
	if err := os.MkdirAll(filepath.Dir(controller.logPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(controller.logPath(), []byte("[cpa] spool import failed: status=sync_failed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	failure := controller.classifyFailureLocked(2, false)
	if failure == nil || failure.Code != "cpaMintIncomplete" {
		t.Fatalf("CPA failure = %+v", failure)
	}
}

func TestClassifyFailureReportsExplicitCPAChatProbeFailure(t *testing.T) {
	controller := newControllerTest(t, 0)
	if err := os.MkdirAll(filepath.Dir(controller.logPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(controller.logPath(), []byte("chat probe failed: permission-denied\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	failure := controller.classifyFailureLocked(2, false)
	if failure == nil || failure.Code != "cpaChatProbeFailed" {
		t.Fatalf("chat probe failure = %+v", failure)
	}
}

func environmentValue(environment []string, key string) string {
	prefix := strings.ToUpper(key) + "="
	for _, item := range environment {
		if strings.HasPrefix(strings.ToUpper(item), prefix) {
			return item[len(prefix):]
		}
	}
	return ""
}

func newControllerTest(t *testing.T, sleepMS int) *Controller {
	t.Helper()
	t.Setenv("GO_REGISTRATION_HELPER", "1")
	t.Setenv("GO_REGISTRATION_HELPER_SLEEP_MS", strconv.Itoa(sleepMS))
	root := t.TempDir()
	workdir := filepath.Join(root, "registration")
	dataDir := filepath.Join(root, "data", "registration")
	spool := filepath.Join(dataDir, "spool")
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "protocol_register_cli.py"), []byte("# synthetic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	example := map[string]any{
		"engine":                       "protocol",
		"email_provider":               "yyds",
		"yyds_api_key":                 "synthetic",
		"captcha_solver":               "yescaptcha",
		"yescaptcha_api_key":           "synthetic",
		"cpa_base_url":                 "https://cli-chat-proxy.grok.com/v1",
		"cpa_probe_chat":               true,
		"cpa_close_browser_after_auth": true,
	}
	data, _ := json.Marshal(example)
	if err := os.WriteFile(filepath.Join(workdir, "config.example.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return NewController(nil, Config{
		Enabled: true, SpoolPath: spool, PollInterval: time.Second, WorkDir: workdir,
		ConfigPath: filepath.Join(dataDir, "config.json"),
		Command:    []string{os.Args[0], "-test.run=TestRegistrationHelperProcess", "--"},
	})
}

func prepareBrowserControllerTest(t *testing.T, controller *Controller) {
	t.Helper()
	controller.config.BrowserPath = os.Args[0]
	files := map[string]string{
		"register_cli.py":                                "# synthetic browser worker\n",
		"grok_register_ttk.py":                           "# synthetic browser module\n",
		filepath.Join("turnstilePatch", "manifest.json"): `{}`,
		filepath.Join("turnstilePatch", "content.js"):    "// synthetic\n",
	}
	for name, contents := range files {
		path := filepath.Join(controller.config.WorkDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func waitForStopped(t *testing.T, controller *Controller) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, err := controller.Status()
		if err != nil {
			t.Fatal(err)
		}
		if !status.Running {
			return status
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("registration worker did not stop")
	return Status{}
}
