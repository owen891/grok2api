package registration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func TestDeploymentEnvironmentOverridesWorkerNetworkSettings(t *testing.T) {
	t.Setenv("REGISTRATION_CAPTCHA_ENDPOINT", "http://grok-turnstile-solver:5072")
	t.Setenv("REGISTRATION_PROXY", "")
	controller := newControllerTest(t, 0)
	value := map[string]any{
		"captcha_endpoint": "docker://grokcli-2api:5072",
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

func TestProtocolWorkerArgumentsSupportConfiguredCommandShapes(t *testing.T) {
	script := filepath.Join("registration", "protocol_register_cli.py")
	tests := []struct {
		name    string
		command []string
		want    []string
	}{
		{name: "wrapper", command: []string{"grok2api-registration"}, want: []string{"--protocol-worker", "--help"}},
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
