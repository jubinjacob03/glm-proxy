package zbridge

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// metricsState is plain atomics, so recording costs the request path nothing.
// Exposed via /admin/stats, /health and /metrics.
type metricsState struct {
	startedAt time.Time

	requestsTotal     atomic.Int64
	requestsStreaming atomic.Int64
	requestsFailed    atomic.Int64
	requestsRejected  atomic.Int64
	activeStreams     atomic.Int64
	clientAborts      atomic.Int64

	upstreamAttempts  atomic.Int64
	upstreamUnauth    atomic.Int64
	upstreamErrors    atomic.Int64
	upstreamLatencyNs atomic.Int64
	upstreamLatencyN  atomic.Int64

	sseEvents   atomic.Int64
	sseBytesOut atomic.Int64

	captchaCacheHits   atomic.Int64
	captchaCacheMisses atomic.Int64
	captchaGenerated   atomic.Int64
	captchaFailed      atomic.Int64
	tokensConsumed     atomic.Int64

	collectorRuns     atomic.Int64
	collectorFailures atomic.Int64
	collectorLastRun  atomic.Int64 // unix seconds; 0 = never
}

var metrics = &metricsState{startedAt: time.Now()}

func (m *metricsState) observeUpstreamLatency(d time.Duration) {
	m.upstreamLatencyNs.Add(int64(d))
	m.upstreamLatencyN.Add(1)
}

func (m *metricsState) avgUpstreamLatencyMs() float64 {
	n := m.upstreamLatencyN.Load()
	if n == 0 {
		return 0
	}
	return float64(m.upstreamLatencyNs.Load()) / float64(n) / 1e6
}

// snapshot renders the counters plus live subsystem state.
func (m *metricsState) snapshot() map[string]interface{} {
	cacheDepth, cachePending := captchaCache.stats()
	return map[string]interface{}{
		"uptime_seconds": int64(time.Since(m.startedAt).Seconds()),
		"requests": map[string]interface{}{
			"total":     m.requestsTotal.Load(),
			"streaming": m.requestsStreaming.Load(),
			"failed":    m.requestsFailed.Load(),
			"rejected":  m.requestsRejected.Load(),
			"active":    m.activeStreams.Load(),
			"aborted":   m.clientAborts.Load(),
		},
		"upstream": map[string]interface{}{
			"attempts":       m.upstreamAttempts.Load(),
			"unauthorized":   m.upstreamUnauth.Load(),
			"errors":         m.upstreamErrors.Load(),
			"avg_latency_ms": m.avgUpstreamLatencyMs(),
			"sse_events":     m.sseEvents.Load(),
			"sse_bytes_out":  m.sseBytesOut.Load(),
			"max_conns":      config.UpstreamMaxConns,
		},
		"captcha": map[string]interface{}{
			"cache_hits":      m.captchaCacheHits.Load(),
			"cache_misses":    m.captchaCacheMisses.Load(),
			"cache_depth":     cacheDepth,
			"cache_pending":   cachePending,
			"generated":       m.captchaGenerated.Load(),
			"failed":          m.captchaFailed.Load(),
			"tokens_consumed": m.tokensConsumed.Load(),
		},
		"device_tokens": map[string]interface{}{
			"available":          getTokenCount(),
			"low_watermark":      config.TokenMonitor.MinTokens,
			"collector_runs":     m.collectorRuns.Load(),
			"collector_failures": m.collectorFailures.Load(),
			"collector_last_run": m.collectorLastRun.Load(),
		},
		"session_pool": sessionPoolStatus(),
	}
}

// Prometheus text exposition for /metrics.

type promSample struct {
	name  string
	help  string
	kind  string
	value float64
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	cacheDepth, cachePending := captchaCache.stats()
	session.mu.Lock()
	sessionReady := session.Initialized
	session.mu.Unlock()

	poolReady, poolSize := 0, 0
	if sessionPool != nil {
		poolReady, poolSize = sessionPool.Ready(), sessionPool.Size()
	}

	samples := []promSample{
		{"zai_uptime_seconds", "Process uptime in seconds.", "gauge", time.Since(metrics.startedAt).Seconds()},
		{"zai_requests_total", "Completion requests accepted.", "counter", float64(metrics.requestsTotal.Load())},
		{"zai_requests_streaming_total", "Completion requests served as SSE streams.", "counter", float64(metrics.requestsStreaming.Load())},
		{"zai_requests_failed_total", "Completion requests that ended in an error.", "counter", float64(metrics.requestsFailed.Load())},
		{"zai_requests_rejected_total", "Requests rejected before reaching upstream.", "counter", float64(metrics.requestsRejected.Load())},
		{"zai_requests_aborted_total", "Requests abandoned by the client mid-stream.", "counter", float64(metrics.clientAborts.Load())},
		{"zai_active_streams", "Streams currently in flight.", "gauge", float64(metrics.activeStreams.Load())},
		{"zai_upstream_attempts_total", "Upstream chat completion attempts.", "counter", float64(metrics.upstreamAttempts.Load())},
		{"zai_upstream_unauthorized_total", "Upstream responses with HTTP 401.", "counter", float64(metrics.upstreamUnauth.Load())},
		{"zai_upstream_errors_total", "Upstream attempts that failed.", "counter", float64(metrics.upstreamErrors.Load())},
		{"zai_upstream_latency_ms_avg", "Mean time to upstream response headers.", "gauge", metrics.avgUpstreamLatencyMs()},
		{"zai_sse_events_total", "SSE events forwarded to clients.", "counter", float64(metrics.sseEvents.Load())},
		{"zai_sse_bytes_out_total", "Bytes written to clients over SSE.", "counter", float64(metrics.sseBytesOut.Load())},
		{"zai_captcha_cache_hits_total", "Captcha parameters served from cache.", "counter", float64(metrics.captchaCacheHits.Load())},
		{"zai_captcha_cache_misses_total", "Captcha parameters generated on demand.", "counter", float64(metrics.captchaCacheMisses.Load())},
		{"zai_captcha_cache_depth", "Captcha parameters currently cached.", "gauge", float64(cacheDepth)},
		{"zai_captcha_cache_pending", "Captcha parameters currently being generated.", "gauge", float64(cachePending)},
		{"zai_captcha_generated_total", "Captcha parameters generated successfully.", "counter", float64(metrics.captchaGenerated.Load())},
		{"zai_captcha_failed_total", "Captcha generation failures.", "counter", float64(metrics.captchaFailed.Load())},
		{"zai_device_tokens_consumed_total", "Device tokens spent on captcha verification.", "counter", float64(metrics.tokensConsumed.Load())},
		{"zai_device_tokens_available", "Device tokens remaining in the local store.", "gauge", float64(getTokenCount())},
		{"zai_collector_runs_total", "Token collector invocations.", "counter", float64(metrics.collectorRuns.Load())},
		{"zai_collector_failures_total", "Token collector invocations that failed.", "counter", float64(metrics.collectorFailures.Load())},
		{"zai_collector_last_run_timestamp", "Unix time of the last collector run.", "gauge", float64(metrics.collectorLastRun.Load())},
		{"zai_session_ready", "1 when the upstream session is initialised.", "gauge", boolGauge(sessionReady)},
		{"zai_session_pool_ready", "Pre-made chat sessions ready to hand out.", "gauge", float64(poolReady)},
		{"zai_session_pool_size", "Configured session pool size.", "gauge", float64(poolSize)},
	}

	var sb strings.Builder
	sb.Grow(4096)
	for _, s := range samples {
		fmt.Fprintf(&sb, "# HELP %s %s\n# TYPE %s %s\n%s %s\n",
			s.name, s.help, s.name, s.kind, s.name,
			strconv.FormatFloat(s.value, 'g', -1, 64))
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(sb.String()))
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
