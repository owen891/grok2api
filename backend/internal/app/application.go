package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	accountapp "github.com/owen891/grok2api/backend/internal/application/account"
	inspectionapp "github.com/owen891/grok2api/backend/internal/application/accountinspection"
	accountsyncapp "github.com/owen891/grok2api/backend/internal/application/accountsync"
	"github.com/owen891/grok2api/backend/internal/application/adminauth"
	auditapp "github.com/owen891/grok2api/backend/internal/application/audit"
	clientkeyapp "github.com/owen891/grok2api/backend/internal/application/clientkey"
	dashboardapp "github.com/owen891/grok2api/backend/internal/application/dashboard"
	egressapp "github.com/owen891/grok2api/backend/internal/application/egress"
	egressgroupapp "github.com/owen891/grok2api/backend/internal/application/egressgroup"
	"github.com/owen891/grok2api/backend/internal/application/gateway"
	mediaapp "github.com/owen891/grok2api/backend/internal/application/media"
	modelapp "github.com/owen891/grok2api/backend/internal/application/model"
	operationsapp "github.com/owen891/grok2api/backend/internal/application/operations"
	quotarecoveryapp "github.com/owen891/grok2api/backend/internal/application/quotarecovery"
	registrationapp "github.com/owen891/grok2api/backend/internal/application/registration"
	replenishmentapp "github.com/owen891/grok2api/backend/internal/application/replenishment"
	settingsapp "github.com/owen891/grok2api/backend/internal/application/settings"
	updatecheckapp "github.com/owen891/grok2api/backend/internal/application/updatecheck"
	"github.com/owen891/grok2api/backend/internal/buildinfo"
	"github.com/owen891/grok2api/backend/internal/domain/account"
	"github.com/owen891/grok2api/backend/internal/infra/config"
	infraegress "github.com/owen891/grok2api/backend/internal/infra/egress"
	inframedia "github.com/owen891/grok2api/backend/internal/infra/media"
	"github.com/owen891/grok2api/backend/internal/infra/persistence/relational"
	"github.com/owen891/grok2api/backend/internal/infra/provider"
	cliprovider "github.com/owen891/grok2api/backend/internal/infra/provider/cli"
	consoleprovider "github.com/owen891/grok2api/backend/internal/infra/provider/console"
	webprovider "github.com/owen891/grok2api/backend/internal/infra/provider/web"
	"github.com/owen891/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/owen891/grok2api/backend/internal/infra/runtime/redis"
	"github.com/owen891/grok2api/backend/internal/infra/security"
	"github.com/owen891/grok2api/backend/internal/observability"
	"github.com/owen891/grok2api/backend/internal/pkg/batch"
	"github.com/owen891/grok2api/backend/internal/repository"
	httpserver "github.com/owen891/grok2api/backend/internal/transport/http"
)

// Application 管理后端进程生命周期和本地后台任务。
type Application struct {
	logger             *slog.Logger
	database           *relational.Database
	server             *http.Server
	audits             *auditapp.Service
	responses          repository.ResponseRepository
	runtime            io.Closer
	settingsBus        repository.SettingsChangeBus
	settings           *settingsapp.Service
	gateway            *gateway.Service
	media              *mediaapp.Service
	quotaRecovery      *quotarecoveryapp.Service
	replenisher        *replenishmentapp.Service
	registrationSpool  *registrationapp.Service
	registration       *registrationapp.Controller
	accounts           *accountapp.Service
	accountInspections *inspectionapp.Service
	models             *modelapp.Service
	operations         *operationsapp.Service
	clientKeys         *clientkeyapp.Service
	accountRepo        repository.AccountRepository
	modelRepo          repository.ModelRepository
	providers          *provider.Registry
	web                *webprovider.Adapter
	startup            *startupState
}

func resolveRegistrationProxyGroup(ctx context.Context, groups repository.EgressGroupRepository, cipher *security.Cipher, groupID uint64, expectedScope string, visited map[uint64]struct{}) ([]string, error) {
	if groupID == 0 {
		return nil, nil
	}
	if _, exists := visited[groupID]; exists {
		return nil, fmt.Errorf("registration proxy group fallback cycle")
	}
	visited[groupID] = struct{}{}
	group, err := groups.GetEgressGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if expectedScope != "" && string(group.Scope) != expectedScope {
		return nil, fmt.Errorf("registration proxy group scope %q does not match %q", group.Scope, expectedScope)
	}
	if !group.Enabled {
		if group.FallbackGroupID == nil {
			return nil, nil
		}
		return resolveRegistrationProxyGroup(ctx, groups, cipher, *group.FallbackGroupID, expectedScope, visited)
	}
	nodeRepository, ok := groups.(repository.EgressRepository)
	if !ok {
		return nil, fmt.Errorf("registration proxy group node repository is unavailable")
	}
	members, err := groups.ListEgressGroupMembers(ctx, groupID)
	if err != nil {
		return nil, err
	}
	allowed := make(map[uint64]struct{}, len(members))
	for _, member := range members {
		if member.Enabled {
			allowed[member.NodeID] = struct{}{}
		}
	}
	nodes, err := nodeRepository.ListEgressNodes(ctx, group.Scope, repository.SortQuery{})
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	proxies := make([]string, 0, len(allowed))
	for _, node := range nodes {
		if _, ok := allowed[node.ID]; !ok || !node.Enabled || node.EncryptedProxyURL == "" || (node.CooldownUntil != nil && now.Before(*node.CooldownUntil)) {
			continue
		}
		proxyURL, err := cipher.Decrypt(node.EncryptedProxyURL)
		if err != nil {
			return nil, err
		}
		normalized, err := egressapp.NormalizeProxyURL(proxyURL)
		if err != nil {
			return nil, err
		}
		if normalized != "" {
			proxies = append(proxies, normalized)
		}
	}
	if len(proxies) > 0 || group.FallbackGroupID == nil {
		return proxies, nil
	}
	return resolveRegistrationProxyGroup(ctx, groups, cipher, *group.FallbackGroupID, expectedScope, visited)
}

// New 完成数据库、Provider、应用服务和 HTTP 路由装配。
func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*Application, error) {
	var database *relational.Database
	var err error
	switch cfg.Database.Driver {
	case "sqlite":
		database, err = relational.OpenSQLite(ctx, cfg.Database.SQLite.Path)
	case "postgres":
		database, err = relational.OpenPostgres(ctx, cfg.Database.Postgres.DSN, cfg.Database.Postgres.MaxOpenConns, cfg.Database.Postgres.MaxIdleConns)
	default:
		return nil, fmt.Errorf("不支持的数据库驱动: %s", cfg.Database.Driver)
	}
	if err != nil {
		return nil, err
	}
	if err := database.InitializeSchema(ctx); err != nil {
		database.Close()
		return nil, err
	}
	cipher, err := security.NewCipher(cfg.Secrets.CredentialEncryptionKey)
	if err != nil {
		database.Close()
		return nil, err
	}

	adminRepo := relational.NewAdminRepository(database)
	sessionRepo := relational.NewAdminSessionRepository(database)
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	clientKeyRepo := relational.NewClientKeyRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	dashboardRepo := relational.NewDashboardRepository(database)
	runtimeSettingsRepo := relational.NewRuntimeSettingsRepository(database, cipher)
	egressRepo := relational.NewEgressRepository(database)
	egressGroupService := egressgroupapp.NewService(egressRepo, egressRepo, cipher)
	mediaJobRepo := relational.NewMediaJobRepository(database)
	mediaAssetRepo := relational.NewMediaAssetRepository(database)
	replenishmentRepo := relational.NewReplenishmentRepository(database)
	accountInspectionRepo := relational.NewAccountInspectionRepository(database)
	loadedConfig, settingsUpdatedAt, settingsRevision, err := settingsapp.LoadPersisted(ctx, cfg, runtimeSettingsRepo)
	if err != nil {
		database.Close()
		return nil, err
	}
	cfg = loadedConfig
	localMediaStore, err := inframedia.NewLocalStore(cfg.Media.Local.Path)
	if err != nil {
		database.Close()
		return nil, err
	}
	var rateLimiter repository.RateLimiter
	var concurrency repository.ConcurrencyLimiter
	var sticky repository.StickySessionRepository
	var deviceSessions repository.DeviceSessionRepository
	var refreshLock repository.DistributedLock
	var settingsBus repository.SettingsChangeBus
	var quotaQueue repository.QuotaRecoveryQueue
	var routePerformance repository.RoutePerformanceRepository
	var runtimeStore io.Closer
	runtimeHealth := func(context.Context) error { return nil }
	switch cfg.RuntimeStore.Driver {
	case "redis":
		redisStore, openErr := redisruntime.Open(ctx, redisruntime.Config{
			Address: cfg.RuntimeStore.Redis.Address, Username: cfg.RuntimeStore.Redis.Username,
			Password: cfg.RuntimeStore.Redis.Password, Database: cfg.RuntimeStore.Redis.Database,
			KeyPrefix: cfg.RuntimeStore.Redis.KeyPrefix, TLS: cfg.RuntimeStore.Redis.TLS,
			ConcurrencyLease: cfg.Server.RequestTimeout.Value() + time.Minute,
		})
		if openErr != nil {
			database.Close()
			return nil, openErr
		}
		runtimeStore = redisStore
		runtimeHealth = redisStore.Ping
		rateLimiter = redisStore
		concurrency = redisruntime.NewConcurrencyLimiter(redisStore)
		sticky = redisStore
		deviceSessions = redisruntime.NewDeviceSessionStore(redisStore)
		refreshLock = redisruntime.NewLockStore(redisStore)
		settingsBus = redisStore
		quotaQueue = redisStore
		routePerformance = redisStore
	case "memory":
		rateLimiter = memory.NewRateLimiter()
		concurrency = memory.NewConcurrencyLimiter()
		sticky = memory.NewStickyStore()
		deviceSessions = memory.NewDeviceSessionStore()
		refreshLock = memory.NewLockStore()
		quotaQueue = memory.NewQuotaRecoveryQueue()
		routePerformance = memory.NewRoutePerformanceStore()
	default:
		database.Close()
		return nil, fmt.Errorf("不支持的运行态驱动: %s", cfg.RuntimeStore.Driver)
	}
	mediaService := mediaapp.NewService(mediaAssetRepo, mediaJobRepo, localMediaStore, refreshLock, mediaConfig(cfg))

	egressManager := infraegress.NewManager(egressRepo, cipher, concurrency)
	cliAdapter := cliprovider.NewAdapter(cliprovider.Config{BaseURL: cfg.Provider.Build.BaseURL, ClientVersion: cfg.Provider.Build.ClientVersion, ClientIdentifier: cfg.Provider.Build.ClientIdentifier, TokenAuth: cfg.Provider.Build.TokenAuth, UserAgent: cfg.Provider.Build.UserAgent}, cipher)
	cliAdapter.SetEgress(egressManager)
	webAdapter := webprovider.NewAdapter(webProviderConfig(cfg), egressManager, cipher, responseRepo, mediaService)
	webAdapter.SetLogger(logger)
	consoleAdapter := consoleprovider.NewAdapter(consoleProviderConfig(cfg), egressManager, cipher)
	providers := provider.NewRegistry(cliAdapter, webAdapter, consoleAdapter)
	if err := providers.Validate(); err != nil {
		if runtimeStore != nil {
			_ = runtimeStore.Close()
		}
		database.Close()
		return nil, fmt.Errorf("校验 Provider 注册表: %w", err)
	}
	adminService := adminauth.NewService(adminRepo, sessionRepo, security.NewTokenService(cfg.Secrets.JWTSecret), cfg.Auth.AccessTokenTTL.Value(), cfg.Auth.RefreshTokenTTL.Value())
	adminService.SetLoginRateLimiter(rateLimiter)
	if err := adminService.Bootstrap(ctx, cfg.BootstrapAdmin.Username, cfg.BootstrapAdmin.Password); err != nil {
		if runtimeStore != nil {
			_ = runtimeStore.Close()
		}
		database.Close()
		return nil, err
	}
	bulkPool := batch.NewSharedPool(maxBatchConcurrency(cfg.Batch), concurrency, "bulk:upstream")
	importPool := batch.NewSharedChildPool(cfg.Batch.ImportConcurrency, concurrency, "bulk:import", bulkPool)
	conversionPool := batch.NewSharedChildPool(cfg.Batch.ConversionConcurrency, concurrency, "bulk:conversion", bulkPool)
	syncPool := batch.NewSharedChildPool(cfg.Batch.SyncConcurrency, concurrency, "bulk:sync", bulkPool)
	refreshPool := batch.NewSharedChildPool(cfg.Batch.RefreshConcurrency, concurrency, "bulk:refresh", bulkPool)
	for _, pool := range []*batch.Pool{importPool, conversionPool, syncPool, refreshPool} {
		pool.UpdateJitter(cfg.Batch.RandomDelay.Value())
	}
	accountService := accountapp.NewService(accountRepo, auditRepo, deviceSessions, sticky, providers, cipher, refreshLock)
	accountService.SetLogger(logger)
	accountService.SetQuotaRecoveryQueue(quotaQueue)
	accountService.SetTaskPools(conversionPool, syncPool, refreshPool)
	windows, err := accountRepo.ListQuotaRecoveryWindows(ctx, 100000)
	if err != nil {
		if runtimeStore != nil {
			_ = runtimeStore.Close()
		}
		database.Close()
		return nil, fmt.Errorf("加载 Web 额度恢复事件: %w", err)
	}
	for _, window := range windows {
		if window.ResetAt != nil {
			if err := quotaQueue.ScheduleQuotaRecovery(ctx, account.QuotaRecoveryEvent{AccountID: window.AccountID, Mode: window.Mode, DueAt: *window.ResetAt}); err != nil {
				if runtimeStore != nil {
					_ = runtimeStore.Close()
				}
				database.Close()
				return nil, fmt.Errorf("恢复 Web 额度事件: %w", err)
			}
		}
	}
	modelService := modelapp.NewService(modelRepo, accountRepo, accountService, providers, egressRepo)
	modelService.SetBulkPool(syncPool)
	modelService.SetLogger(logger)
	if err := modelRepo.ReplaceProviderRoutes(ctx, account.ProviderWeb, webprovider.Routes()); err != nil {
		if runtimeStore != nil {
			_ = runtimeStore.Close()
		}
		database.Close()
		return nil, fmt.Errorf("初始化 Grok Web 模型目录: %w", err)
	}
	if err := modelRepo.ReplaceProviderRoutes(ctx, account.ProviderConsole, consoleprovider.Routes()); err != nil {
		if runtimeStore != nil {
			_ = runtimeStore.Close()
		}
		database.Close()
		return nil, fmt.Errorf("初始化 Grok Console 模型目录: %w", err)
	}
	accountSyncService := accountsyncapp.NewService(logger, accountService, accountService, accountService, modelService)
	accountSyncService.SetBulkPool(importPool)
	accountSyncService.UpdateConcurrency(cfg.Batch.ImportConcurrency)
	var registrationSpool *registrationapp.Service
	registrationConfig := registrationapp.Config{
		Enabled: cfg.Registration.Enabled, SpoolPath: cfg.Registration.SpoolPath, PollInterval: cfg.Registration.PollInterval.Value(),
		FailedRetention: cfg.Registration.FailedRetention.Value(),
		WorkDir:         cfg.Registration.WorkDir, ConfigPath: cfg.Registration.ConfigPath,
		Command: append([]string(nil), cfg.Registration.Command...), BrowserMode: cfg.Registration.BrowserMode, BrowserPath: cfg.Registration.BrowserPath,
		ResolveProxyGroup: func(resolveCtx context.Context, groupID uint64, expectedScope string) ([]string, error) {
			return resolveRegistrationProxyGroup(resolveCtx, egressRepo, cipher, groupID, expectedScope, make(map[uint64]struct{}))
		},
	}
	registrationController := registrationapp.NewController(logger, registrationConfig)
	if cfg.Registration.Enabled {
		registrationSpool = registrationapp.NewService(logger, accountService, accountSyncService, registrationConfig, accountService)
	}
	egressService := egressapp.NewService(egressRepo, cipher, infraegress.DefaultUserAgent, cfg.Provider.Console.UserAgent)
	clientKeyService := clientkeyapp.NewService(clientKeyRepo, rateLimiter, concurrency, cfg.ClientKeyDefaults.RPMLimit, cfg.ClientKeyDefaults.MaxConcurrent, cipher)
	auditService := auditapp.NewService(auditRepo, logger, cfg.Audit.BufferSize, cfg.Audit.BatchSize, cfg.Audit.FlushInterval.Value())
	dashboardService := dashboardapp.NewService(dashboardRepo)
	selector := gateway.NewSelector(accountRepo, concurrency, sticky, providers, cfg.Routing.StickyTTL.Value(), cfg.Routing.CooldownBase.Value(), cfg.Routing.CooldownMax.Value(), cfg.Routing.CapacityWait.Value())
	selector.SetLogger(logger)
	selector.SetRoutePerformanceRepository(routePerformance)
	accountInspectionService := inspectionapp.NewService(accountRepo, modelRepo, accountInspectionRepo, providers, accountService, selector, logger)
	gatewayService := gateway.NewService(modelService, auditService, accountService, clientKeyService, providers, selector, responseRepo, cfg.Routing.MaxAttempts)
	gatewayService.SetLogger(logger)
	gatewayService.ConfigureMedia(mediaJobRepo, cfg.Provider.Web.MediaConcurrency)
	quotaRecoveryService := quotarecoveryapp.NewService(logger, quotaQueue, accountService, cfg.Provider.Web.RecoveryBackoffBase.Value(), cfg.Provider.Web.RecoveryBackoffMax.Value())
	quotaRecoveryService.SetBulkPool(syncPool)
	replenisher := replenishmentapp.NewService(selector, replenishmentRepo, registrationController, replenishmentapp.Config{
		Enabled: cfg.Registration.AutoReplenish.Enabled, DryRun: cfg.Registration.AutoReplenish.DryRun,
		Provider: account.Provider(cfg.Registration.AutoReplenish.Provider), Model: cfg.Registration.AutoReplenish.Model,
		QuotaMode: cfg.Registration.AutoReplenish.QuotaMode, RegisterCount: cfg.Registration.AutoReplenish.RegisterCount,
		Cooldown: cfg.Registration.AutoReplenish.Cooldown.Value(), RecoveryLeadTime: cfg.Registration.AutoReplenish.RecoveryLeadTime.Value(),
		Predictive: cfg.Registration.AutoReplenish.Predictive, TargetEligible: cfg.Registration.AutoReplenish.TargetEligible,
		MinDemandRPM: cfg.Registration.AutoReplenish.MinDemandRPM, DemandWindow: cfg.Registration.AutoReplenish.DemandWindow.Value(),
		VerificationGrace:     cfg.Registration.AutoReplenish.VerificationGrace.Value(),
		MaxDailyRegistrations: cfg.Registration.AutoReplenish.MaxDailyRegistrations,
	}, logger)
	replenisher.SetDemandSource(auditRepo)
	gatewayService.SetCapacityReplenisher(replenisher)
	operationsService := operationsapp.NewService(modelRepo, selector, providers, replenishmentRepo, operationsapp.ReplenishmentConfig{
		Enabled: cfg.Registration.AutoReplenish.Enabled, DryRun: cfg.Registration.AutoReplenish.DryRun,
		Scope: replenisher.Scope(), MaxDailyRegistrations: cfg.Registration.AutoReplenish.MaxDailyRegistrations,
		Predictive: cfg.Registration.AutoReplenish.Predictive, TargetEligible: cfg.Registration.AutoReplenish.TargetEligible,
		MinDemandRPM: cfg.Registration.AutoReplenish.MinDemandRPM, DemandWindow: cfg.Registration.AutoReplenish.DemandWindow.Value(),
		VerificationGrace: cfg.Registration.AutoReplenish.VerificationGrace.Value(),
	})
	operationsService.SetReplenishmentTrigger(replenisher)
	var notifySettings func(context.Context)
	if settingsBus != nil {
		notifySettings = func(notifyCtx context.Context) {
			publishCtx, cancel := context.WithTimeout(context.WithoutCancel(notifyCtx), 3*time.Second)
			defer cancel()
			if err := settingsBus.PublishSettingsChanged(publishCtx); err != nil {
				logger.Warn("settings_change_publish_failed", "error", err)
			}
		}
	}
	settingsService := settingsapp.NewService(cfg, settingsUpdatedAt, settingsRevision, runtimeSettingsRepo, notifySettings, func(next config.Config) {
		bulkPool.UpdateLimit(maxBatchConcurrency(next.Batch))
		importPool.UpdateLimit(next.Batch.ImportConcurrency)
		conversionPool.UpdateLimit(next.Batch.ConversionConcurrency)
		syncPool.UpdateLimit(next.Batch.SyncConcurrency)
		refreshPool.UpdateLimit(next.Batch.RefreshConcurrency)
		for _, pool := range []*batch.Pool{importPool, conversionPool, syncPool, refreshPool} {
			pool.UpdateJitter(next.Batch.RandomDelay.Value())
		}
		cliAdapter.UpdateConfig(cliprovider.Config{
			BaseURL: next.Provider.Build.BaseURL, ClientVersion: next.Provider.Build.ClientVersion,
			ClientIdentifier: next.Provider.Build.ClientIdentifier, TokenAuth: next.Provider.Build.TokenAuth,
			UserAgent: next.Provider.Build.UserAgent,
		})
		webAdapter.UpdateConfig(webProviderConfig(next))
		consoleAdapter.UpdateConfig(consoleProviderConfig(next))
		egressService.UpdateDefaults(infraegress.DefaultUserAgent, next.Provider.Console.UserAgent)
		mediaService.UpdateConfig(mediaConfig(next))
		quotaRecoveryService.UpdateConfig(next.Provider.Web.RecoveryBackoffBase.Value(), next.Provider.Web.RecoveryBackoffMax.Value())
		accountSyncService.UpdateConcurrency(next.Batch.ImportConcurrency)
		selector.UpdateConfig(next.Routing.StickyTTL.Value(), next.Routing.CooldownBase.Value(), next.Routing.CooldownMax.Value(), next.Routing.CapacityWait.Value())
		gatewayService.UpdateMaxAttempts(next.Routing.MaxAttempts)
		auditService.UpdateConfig(next.Audit.BatchSize, next.Audit.FlushInterval.Value())
		clientKeyService.UpdateDefaults(next.ClientKeyDefaults.RPMLimit, next.ClientKeyDefaults.MaxConcurrent)
	})
	versionCheckService := updatecheckapp.NewService(buildinfo.CurrentVersion(), nil)

	startup := newStartupState(len(windows))
	readiness := func(readyCtx context.Context) httpserver.ReadinessSnapshot {
		return readinessSnapshot(readyCtx, startup, runtimeHealth, modelRepo, accountRepo, providers)
	}
	var metricsHandler http.Handler
	if cfg.Server.MetricsEnabled {
		metricsHandler = observability.Handler()
	}
	router := httpserver.New(httpserver.Dependencies{Logger: logger, RequestTimeout: cfg.Server.RequestTimeout.Value(), MaxBodyBytes: cfg.Server.MaxBodyBytes, SecureCookies: cfg.Auth.SecureCookies, SwaggerEnabled: cfg.Server.SwaggerEnabled, PublicAPIBaseURL: cfg.Frontend.PublicAPIBaseURL, FrontendStaticPath: cfg.Frontend.StaticPath, Readiness: readiness, TrafficReady: startup.acceptsTraffic, AdminAuth: adminService, Accounts: accountService, AccountInspections: accountInspectionService, AccountSync: accountSyncService, Models: modelService, Operations: operationsService, ClientKeys: clientKeyService, Audits: auditService, Dashboard: dashboardService, Gateway: gatewayService, Media: mediaService, Settings: settingsService, UpdateCheck: versionCheckService, Egress: egressService, EgressGroups: egressGroupService, Registration: registrationController, Metrics: metricsHandler})
	server := &http.Server{Addr: cfg.Server.Listen, Handler: router, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: cfg.Server.ReadTimeout.Value(), IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
	return &Application{
		logger: logger, database: database, server: server,
		audits: auditService, responses: responseRepo, runtime: runtimeStore,
		settingsBus: settingsBus, settings: settingsService, gateway: gatewayService, media: mediaService, quotaRecovery: quotaRecoveryService, replenisher: replenisher, registrationSpool: registrationSpool, registration: registrationController, accounts: accountService, accountInspections: accountInspectionService, models: modelService, operations: operationsService, clientKeys: clientKeyService,
		accountRepo: accountRepo, modelRepo: modelRepo, providers: providers, web: webAdapter, startup: startup,
	}, nil
}

func maxBatchConcurrency(value config.BatchConfig) int {
	return max(value.ImportConcurrency, value.ConversionConcurrency, value.SyncConcurrency, value.RefreshConcurrency)
}

func webProviderConfig(cfg config.Config) webprovider.Config {
	return webprovider.Config{
		BaseURL: cfg.Provider.Web.BaseURL, BrowserWorkerURL: cfg.Provider.Web.BrowserWorkerURL,
		QuotaTimeoutSeconds: int(cfg.Provider.Web.QuotaTimeout.Value().Seconds()),
		StatsigMode:         cfg.Provider.Web.StatsigMode, StatsigManualValue: cfg.Provider.Web.StatsigManualValue,
		StatsigSignerURL:   cfg.Provider.Web.StatsigSignerURL,
		ChatTimeoutSeconds: int(cfg.Provider.Web.ChatTimeout.Value().Seconds()), ImageTimeoutSeconds: int(cfg.Provider.Web.ImageTimeout.Value().Seconds()),
		VideoTimeoutSeconds: int(cfg.Provider.Web.VideoTimeout.Value().Seconds()), MaxInputImageBytes: cfg.Media.MaxImageBytes,
		AllowNSFW: cfg.Provider.Web.AllowNSFW,
	}
}

func consoleProviderConfig(cfg config.Config) consoleprovider.Config {
	return consoleprovider.Config{
		BaseURL: cfg.Provider.Console.BaseURL, UserAgent: cfg.Provider.Console.UserAgent,
		TimeoutSeconds: int(cfg.Provider.Console.ChatTimeout.Value().Seconds()),
	}
}

func mediaConfig(cfg config.Config) mediaapp.Config {
	return mediaapp.Config{
		PublicBaseURL: cfg.Frontend.PublicAPIBaseURL, MaxImageBytes: cfg.Media.MaxImageBytes,
		MaxTotalBytes: cfg.Media.MaxTotalBytes, CleanupThresholdPercent: cfg.Media.CleanupThresholdPercent,
		CleanupInterval: cfg.Media.CleanupInterval.Value(),
	}
}

// Run 启动 HTTP 服务和本地后台维护任务。
func (a *Application) Run(ctx context.Context) error {
	if a.registration != nil {
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := a.registration.Close(stopCtx); err != nil {
				a.logger.Warn("registration_shutdown_failed", "error", err)
			}
		}()
	}
	a.audits.Start()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := a.audits.Close(closeCtx); err != nil {
			a.logger.Warn("audit_shutdown_failed", "error", err)
		}
	}()
	runCtx, cancelBackground := context.WithCancel(ctx)
	var background sync.WaitGroup
	defer func() {
		cancelBackground()
		background.Wait()
	}()
	errCh := make(chan error, 1)
	go func() {
		a.logger.Info("server_started", "listen", a.server.Addr)
		errCh <- a.server.ListenAndServe()
	}()
	a.reconcileStartup(runCtx)
	startBackground := func(name string, task func(context.Context) error) {
		background.Add(1)
		go func() {
			defer background.Done()
			a.runSupervisedTask(runCtx, name, task)
		}()
	}
	startBackground("settings_reconcile", func(taskCtx context.Context) error {
		a.runPeriodicTask(taskCtx, 30*time.Second, "settings_reconcile", func(runCtx context.Context) error {
			return a.settings.ReloadPersisted(runCtx)
		})
		return nil
	})
	startBackground("billing_reservation_cleanup", func(taskCtx context.Context) error {
		a.runPeriodicTask(taskCtx, 10*time.Minute, "billing_reservation_cleanup", func(runCtx context.Context) error {
			_, err := a.clientKeys.CleanupExpiredBilling(runCtx, 1000)
			return err
		})
		return nil
	})
	startBackground("model_cooldown_cleanup", func(taskCtx context.Context) error {
		a.runPeriodicTask(taskCtx, 10*time.Minute, "model_cooldown_cleanup", func(runCtx context.Context) error {
			_, err := a.accountRepo.PruneExpiredModelQuotaBlocks(runCtx, time.Now().UTC(), 1000)
			return err
		})
		return nil
	})
	startBackground("response_ownership_cleanup", func(taskCtx context.Context) error {
		a.runPeriodicTask(taskCtx, 24*time.Hour, "response_ownership_cleanup", func(runCtx context.Context) error {
			_, err := a.responses.DeleteExpired(runCtx, time.Now().UTC())
			return err
		})
		return nil
	})
	startBackground("quota_recovery", func(taskCtx context.Context) error {
		a.quotaRecovery.Run(taskCtx)
		return nil
	})
	if a.replenisher != nil {
		startBackground("registration_replenisher", a.replenisher.Run)
	}
	if a.accountInspections != nil {
		startBackground("account_inspection", a.accountInspections.Run)
	}
	if a.operations != nil {
		startBackground("operations_sampler", a.runOperationsSampler)
	}
	if a.registrationSpool != nil {
		startBackground("registration_spool", a.registrationSpool.Run)
	}
	startBackground("web_quota_refresh", func(taskCtx context.Context) error {
		a.accounts.RunWebQuotaRefresh(taskCtx)
		return nil
	})
	startBackground("credential_refresh", func(taskCtx context.Context) error {
		a.accounts.RunCredentialRefresh(taskCtx)
		return nil
	})
	startBackground("statsig_warmup", func(taskCtx context.Context) error {
		a.runStatsigWarmup(taskCtx)
		return nil
	})
	startBackground("web_quota_startup_catchup", func(taskCtx context.Context) error {
		a.runWebQuotaCatchup(taskCtx)
		return nil
	})
	startBackground("model_catalog_startup_catchup", func(taskCtx context.Context) error {
		a.runModelCatalogCatchup(taskCtx)
		return nil
	})
	startBackground("video_recovery", func(taskCtx context.Context) error {
		a.gateway.RunVideoRecovery(taskCtx)
		return nil
	})
	startBackground("video_workers", func(taskCtx context.Context) error {
		a.gateway.RunVideoWorkers(taskCtx)
		return nil
	})
	startBackground("media_cleanup", func(taskCtx context.Context) error {
		a.media.RunCleanup(taskCtx, func(err error) {
			a.logger.Warn("media_cleanup_failed", "error", err)
		})
		return nil
	})
	if a.settingsBus != nil {
		startBackground("settings_change_listener", func(taskCtx context.Context) error {
			return a.settingsBus.ListenSettingsChanges(taskCtx, func(eventCtx context.Context) error {
				reloadCtx, cancel := context.WithTimeout(eventCtx, 5*time.Second)
				defer cancel()
				if err := a.settings.ReloadPersisted(reloadCtx); err != nil {
					a.logger.Warn("settings_reload_failed", "error", err)
				}
				return nil
			})
		})
	}
	a.queueDueWebQuotaRefresh(runCtx)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := a.server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("关闭 HTTP 服务: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (a *Application) runOperationsSampler(ctx context.Context) error {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		snapshotCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, err := a.operations.Snapshot(snapshotCtx)
		cancel()
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (a *Application) Close() error {
	var runtimeErr error
	if a.runtime != nil {
		runtimeErr = a.runtime.Close()
	}
	return errors.Join(runtimeErr, a.database.Close())
}

func (a *Application) runPeriodicTask(ctx context.Context, interval time.Duration, name string, task func(context.Context) error) {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	if a.operations != nil {
		a.operations.TaskScheduled(name, time.Now().UTC().Add(interval))
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			runCtx, cancel := context.WithTimeout(ctx, minDuration(interval, 5*time.Minute))
			err := task(runCtx)
			cancel()
			nextRunAt := time.Now().UTC().Add(interval)
			if err != nil {
				a.logger.Warn(name+"_failed", "error", err)
				if a.operations != nil {
					a.operations.TaskFailed(name, err, false)
					a.operations.TaskScheduled(name, nextRunAt)
				}
			} else if a.operations != nil {
				a.operations.TaskSucceeded(name, &nextRunAt)
			}
			resetTimer(timer, interval)
		}
	}
}

func (a *Application) runSupervisedTask(ctx context.Context, name string, task func(context.Context) error) {
	backoff := time.Second
	for {
		stopHeartbeat := a.startTaskHeartbeat(ctx, name)
		err := batch.Do(ctx, task)
		stopHeartbeat()
		if ctx.Err() != nil {
			if a.operations != nil {
				a.operations.TaskStopped(name)
			}
			return
		}
		if err == nil {
			err = errors.New("后台任务意外退出")
		}
		if a.operations != nil {
			a.operations.TaskFailed(name, err, true)
		}
		var panicErr *batch.PanicError
		if errors.As(err, &panicErr) {
			a.logger.Error("background_task_restarting", "task", name, "backoff", backoff, "error", panicErr, "stack", string(panicErr.Stack))
		} else {
			a.logger.Error("background_task_restarting", "task", name, "backoff", backoff, "error", err)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (a *Application) startTaskHeartbeat(ctx context.Context, name string) func() {
	if a.operations == nil {
		return func() {}
	}
	a.operations.TaskStarted(name)
	heartbeatCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				a.operations.TaskHeartbeat(name)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func resetTimer(timer *time.Timer, interval time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(interval)
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}
