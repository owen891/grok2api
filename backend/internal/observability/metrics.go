package observability

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	registry     = prometheus.NewRegistry()
	httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grok2api", Name: "http_requests_total", Help: "HTTP requests by stable route, method and status class.",
	}, []string{"route", "method", "status_class"})
	httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "grok2api", Name: "http_request_duration_seconds", Help: "HTTP request latency by stable route and method.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route", "method"})
	routingPools = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "grok2api", Name: "routing_candidate_pool", Help: "Latest candidate pool classification observed on a route.",
	}, []string{"provider", "model", "state"})
	routingSelections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grok2api", Name: "routing_selections_total", Help: "Routing selections by strategy.",
	}, []string{"provider", "model", "selection"})
	shadowSelections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grok2api", Name: "routing_shadow_selections_total", Help: "Performance scorer shadow recommendations and agreement with live routing.",
	}, []string{"provider", "model", "result"})
	routingFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grok2api", Name: "routing_failures_total", Help: "Routing failures by stable failure scope and action.",
	}, []string{"provider", "model", "scope", "action"})
	routeCapacity = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "grok2api", Name: "route_capacity", Help: "Latest route capacity snapshot by classification.",
	}, []string{"provider", "model", "state"})
	circuitFailureObservations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grok2api", Name: "circuit_failure_observations_total", Help: "Account-scoped failures eligible to advance a route circuit.",
	}, []string{"model"})
	replenishmentState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "grok2api", Name: "replenishment_state", Help: "Current persisted automatic replenishment state.",
	}, []string{"scope", "state"})
	replenishmentTriggers = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grok2api", Name: "replenishment_triggers_total", Help: "Automatic replenishment claims by reason and dry-run mode.",
	}, []string{"scope", "reason", "dry_run"})
	replenishmentFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grok2api", Name: "replenishment_failures_total", Help: "Automatic replenishment failures by stable stage.",
	}, []string{"scope", "stage"})
	backgroundTaskState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "grok2api", Name: "background_task_state", Help: "Current background task state.",
	}, []string{"task", "state"})
	backgroundTaskFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grok2api", Name: "background_task_failures_total", Help: "Background task failures.",
	}, []string{"task"})
	backgroundTaskRestarts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grok2api", Name: "background_task_restarts_total", Help: "Supervised background task restarts.",
	}, []string{"task"})
	accountInspectionResults = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grok2api", Name: "account_inspection_results_total", Help: "Completed active account probes by provider and classification.",
	}, []string{"provider", "classification"})
	accountInspectionRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grok2api", Name: "account_inspection_runs_total", Help: "Finished account inspection runs by provider and status.",
	}, []string{"provider", "status"})
	accountInspectionDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "grok2api", Name: "account_inspection_duration_seconds", Help: "Wall-clock duration of finished account inspection runs.",
		Buckets: prometheus.DefBuckets,
	}, []string{"provider", "status"})
	accountInspectionActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "grok2api", Name: "account_inspection_active", Help: "Whether this instance currently owns an account inspection run.",
	}, []string{"provider"})
)

func init() {
	registry.MustRegister(
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		httpRequests, httpDuration, routingPools, routingSelections, shadowSelections, routingFailures,
		routeCapacity, circuitFailureObservations, replenishmentState, replenishmentTriggers,
		replenishmentFailures, backgroundTaskState, backgroundTaskFailures, backgroundTaskRestarts,
		accountInspectionResults, accountInspectionRuns, accountInspectionDuration, accountInspectionActive,
	)
}

func ObserveAccountInspectionResult(provider, classification string) {
	accountInspectionResults.WithLabelValues(provider, classification).Inc()
}

func ObserveAccountInspectionRun(provider, status string, duration time.Duration) {
	accountInspectionRuns.WithLabelValues(provider, status).Inc()
	accountInspectionDuration.WithLabelValues(provider, status).Observe(max(0, duration.Seconds()))
}

func SetAccountInspectionActive(provider string, active bool) {
	value := 0.0
	if active {
		value = 1
	}
	accountInspectionActive.WithLabelValues(provider).Set(value)
}

func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
}

func ObserveHTTP(route, method string, status int, duration time.Duration) {
	if route == "" {
		route = "unmatched"
	}
	statusClass := strconv.Itoa(max(0, status)/100) + "xx"
	httpRequests.WithLabelValues(route, method, statusClass).Inc()
	httpDuration.WithLabelValues(route, method).Observe(max(0, duration.Seconds()))
}

func ObserveRoutingPool(provider, model string, values map[string]int) {
	for state, value := range values {
		routingPools.WithLabelValues(provider, model, state).Set(float64(max(0, value)))
	}
}

func ObserveRoutingSelection(provider, model, selection string) {
	routingSelections.WithLabelValues(provider, model, selection).Inc()
}

func ObserveShadowSelection(provider, model, result string) {
	shadowSelections.WithLabelValues(provider, model, result).Inc()
}

func ObserveRoutingFailure(provider, model, scope, action string) {
	routingFailures.WithLabelValues(provider, model, scope, action).Inc()
}

func ObserveRouteCapacity(provider, model string, values map[string]int) {
	for state, value := range values {
		routeCapacity.WithLabelValues(provider, model, state).Set(float64(max(0, value)))
	}
}

func ResetRouteCapacity() { routeCapacity.Reset() }

func ObserveCircuitFailure(model string) {
	circuitFailureObservations.WithLabelValues(model).Inc()
}

var replenishmentStates = []string{"idle", "starting", "running", "verifying", "cooling", "failed"}

func SetReplenishmentState(scope, state string) {
	for _, candidate := range replenishmentStates {
		value := 0.0
		if candidate == state {
			value = 1
		}
		replenishmentState.WithLabelValues(scope, candidate).Set(value)
	}
}

func ObserveReplenishmentTrigger(scope, reason string, dryRun bool) {
	replenishmentTriggers.WithLabelValues(scope, reason, strconv.FormatBool(dryRun)).Inc()
}

func ObserveReplenishmentFailure(scope, stage string) {
	replenishmentFailures.WithLabelValues(scope, stage).Inc()
}

var taskStates = []string{"running", "degraded", "stopped"}

func SetBackgroundTaskState(task, state string) {
	for _, candidate := range taskStates {
		value := 0.0
		if candidate == state {
			value = 1
		}
		backgroundTaskState.WithLabelValues(task, candidate).Set(value)
	}
}

func ObserveBackgroundTaskFailure(task string, restarted bool) {
	backgroundTaskFailures.WithLabelValues(task).Inc()
	if restarted {
		backgroundTaskRestarts.WithLabelValues(task).Inc()
	}
}
