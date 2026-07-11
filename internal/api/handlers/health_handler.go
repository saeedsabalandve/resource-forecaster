package handlers

import (
    "context"
    "encoding/json"
    "net/http"
    "runtime"
    "sync"
    "time"

    "github.com/rs/zerolog/log"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/codes"

    "resource-forecaster/internal/storage/timescale"
)

// # HealthHandler provides comprehensive health checking
// # Implements Kubernetes health probe patterns with dependency checks
type HealthHandler struct {
    storage     *timescale.Client
    startTime   time.Time
    mu          sync.RWMutex
    isReady     bool
    isLive      bool
    dependencies map[string]DependencyChecker
    
    // # Health check history for debugging
    lastChecks map[string]CheckResult
}

// # DependencyChecker interface for pluggable health checks
type DependencyChecker interface {
    Name() string
    Check(ctx context.Context) error
    IsCritical() bool
}

// # CheckResult stores health check results
type CheckResult struct {
    Name      string        `json:"name"`
    Status    string        `json:"status"`    // # "healthy", "degraded", "unhealthy"
    Duration  time.Duration `json:"duration_ms"`
    Error     string        `json:"error,omitempty"`
    CheckedAt time.Time     `json:"checked_at"`
    Critical  bool          `json:"critical"`
}

// # HealthResponse provides detailed system health information
type HealthResponse struct {
    Status       string        `json:"status"`        // # "healthy", "degraded", "unhealthy"
    Version      string        `json:"version"`
    Uptime       string        `json:"uptime"`
    Timestamp    time.Time     `json:"timestamp"`
    Checks       []CheckResult `json:"checks,omitempty"`
    Dependencies []CheckResult `json:"dependencies,omitempty"`
    SystemInfo   SystemInfo    `json:"system_info"`
}

// # SystemInfo contains runtime metrics for debugging
type SystemInfo struct {
    GoVersion    string `json:"go_version"`
    NumGoroutine int    `json:"num_goroutine"`
    NumCPU       int    `json:"num_cpu"`
    MemAlloc     uint64 `json:"mem_alloc_mb"`
    MemTotalAlloc uint64 `json:"mem_total_alloc_mb"`
}

// # NewHealthHandler creates health handler with dependency checks
func NewHealthHandler(storage *timescale.Client) *HealthHandler {
    h := &HealthHandler{
        storage:      storage,
        startTime:    time.Now(),
        isReady:      false,
        isLive:       true,
        dependencies: make(map[string]DependencyChecker),
        lastChecks:   make(map[string]CheckResult),
    }

    // # Register dependency checks
    h.RegisterDependency(&DatabaseChecker{client: storage})
    h.RegisterDependency(&RedisChecker{client: redisClient})
    h.RegisterDependency(&S3Checker{bucketName: "forecaster-models"})

    // # Set ready after dependencies are verified
    go func() {
        time.Sleep(5 * time.Second)
        if err := h.checkAllDependencies(context.Background()); err == nil {
            h.mu.Lock()
            h.isReady = true
            h.mu.Unlock()
            log.Info().Msg("Service is ready to accept traffic")
        }
    }()

    return h
}

// # RegisterDependency adds a dependency checker
func (h *HealthHandler) RegisterDependency(checker DependencyChecker) {
    h.dependencies[checker.Name()] = checker
}

// # LivenessCheck handles GET /health/live
// # Simple check to verify the process is running
func (h *HealthHandler) LivenessCheck(w http.ResponseWriter, r *http.Request) {
    h.mu.RLock()
    isLive := h.isLive
    h.mu.RUnlock()

    if !isLive {
        WriteError(w, http.StatusServiceUnavailable, "not_live", "Service is not live")
        return
    }

    WriteJSON(w, http.StatusOK, map[string]string{
        "status": "alive",
        "time":   time.Now().Format(time.RFC3339),
    })
}

// # ReadinessCheck handles GET /health/ready
// # Verifies all critical dependencies are available
func (h *HealthHandler) ReadinessCheck(w http.ResponseWriter, r *http.Request) {
    tracer := otel.Tracer("health-check")
    ctx, span := tracer.Start(r.Context(), "readiness-check")
    defer span.End()

    h.mu.RLock()
    isReady := h.isReady
    h.mu.RUnlock()

    if !isReady {
        WriteError(w, http.StatusServiceUnavailable, "not_ready", 
            "Service is not ready to accept traffic")
        span.SetStatus(codes.Error, "not ready")
        return
    }

    // # Perform dependency checks
    results := h.checkDependencies(ctx)

    // # Determine overall health
    status := "healthy"
    statusCode := http.StatusOK

    for _, result := range results {
        if result.Status == "unhealthy" && result.Critical {
            status = "unhealthy"
            statusCode = http.StatusServiceUnavailable
            break
        }
        if result.Status == "degraded" {
            status = "degraded"
            statusCode = http.StatusOK // # Still return 200 but indicate degradation
        }
    }

    response := HealthResponse{
        Status:       status,
        Version:      Version,
        Uptime:       time.Since(h.startTime).String(),
        Timestamp:    time.Now(),
        Dependencies: results,
        SystemInfo:   collectSystemInfo(),
    }

    WriteJSON(w, statusCode, response)
    
    if status != "healthy" {
        span.SetStatus(codes.Error, status)
    } else {
        span.SetStatus(codes.Ok, status)
    }
}

// # StartupCheck handles GET /health/startup
// # Used by Kubernetes startup probe to delay liveness/readiness
func (h *HealthHandler) StartupCheck(w http.ResponseWriter, r *http.Request) {
    // # Check if critical initialization is complete
    if !h.isLive {
        WriteError(w, http.StatusServiceUnavailable, "starting", 
            "Service is still initializing")
        return
    }

    WriteJSON(w, http.StatusOK, map[string]string{
        "status":   "started",
        "started_at": h.startTime.Format(time.RFC3339),
    })
}

// # FullHealthCheck handles GET /health (detailed check)
func (h *HealthHandler) FullHealthCheck(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    
    // # Run all health checks
    allChecks := h.checkDependencies(ctx)
    
    // # Add system-level checks
    sysChecks := h.runSystemChecks(ctx)
    allChecks = append(allChecks, sysChecks...)

    status := "healthy"
    for _, check := range allChecks {
        if check.Status == "unhealthy" && check.Critical {
            status = "unhealthy"
            break
        }
    }

    response := HealthResponse{
        Status:     status,
        Version:    Version,
        Uptime:     time.Since(h.startTime).String(),
        Timestamp:  time.Now(),
        Checks:     allChecks,
        SystemInfo: collectSystemInfo(),
    }

    WriteJSON(w, http.StatusOK, response)
}

// # checkDependencies runs all registered dependency checks concurrently
func (h *HealthHandler) checkDependencies(ctx context.Context) []CheckResult {
    var wg sync.WaitGroup
    resultsCh := make(chan CheckResult, len(h.dependencies))

    for _, checker := range h.dependencies {
        wg.Add(1)
        go func(c DependencyChecker) {
            defer wg.Done()
            
            startTime := time.Now()
            err := c.Check(ctx)
            duration := time.Since(startTime)

            result := CheckResult{
                Name:      c.Name(),
                Duration:  duration,
                CheckedAt: time.Now(),
                Critical:  c.IsCritical(),
            }

            if err != nil {
                result.Status = "unhealthy"
                result.Error = err.Error()
                log.Warn().
                    Err(err).
                    Str("dependency", c.Name()).
                    Dur("duration", duration).
                    Msg("Dependency check failed")
            } else {
                result.Status = "healthy"
            }

            resultsCh <- result
        }(checker)
    }

    // # Close channel when all checks complete
    go func() {
        wg.Wait()
        close(resultsCh)
    }()

    var results []CheckResult
    for result := range resultsCh {
        results = append(results, result)
        h.mu.Lock()
        h.lastChecks[result.Name] = result
        h.mu.Unlock()
    }

    return results
}

// # runSystemChecks performs system-level health verifications
func (h *HealthHandler) runSystemChecks(ctx context.Context) []CheckResult {
    checks := []CheckResult{
        {
            Name:      "goroutines",
            Status:    "healthy",
            CheckedAt: time.Now(),
            Duration:  0,
        },
        {
            Name:      "memory_usage",
            Status:    "healthy",
            CheckedAt: time.Now(),
            Duration:  0,
        },
    }

    // # Check goroutine count
    goroutines := runtime.NumGoroutine()
    if goroutines > 10000 {
        checks[0].Status = "unhealthy"
        checks[0].Error = fmt.Sprintf("too many goroutines: %d", goroutines)
        checks[0].Critical = true
    } else if goroutines > 5000 {
        checks[0].Status = "degraded"
        checks[0].Error = fmt.Sprintf("high goroutine count: %d", goroutines)
    }

    // # Check memory usage
    var m runtime.MemStats
    runtime.ReadMemStats(&m)
    memUsageMB := m.Alloc / 1024 / 1024
    if memUsageMB > 1024 { // # Over 1GB
        checks[1].Status = "degraded"
        checks[1].Error = fmt.Sprintf("high memory usage: %d MB", memUsageMB)
    }

    return checks
}

// # checkAllDependencies is used for initial readiness check
func (h *HealthHandler) checkAllDependencies(ctx context.Context) error {
    for _, checker := range h.dependencies {
        if err := checker.Check(ctx); err != nil {
            if checker.IsCritical() {
                return fmt.Errorf("critical dependency %s failed: %w", checker.Name(), err)
            }
            log.Warn().Err(err).Str("dependency", checker.Name()).Msg("Non-critical dependency failed")
        }
    }
    return nil
}

// # DatabaseChecker implements health check for TimescaleDB
type DatabaseChecker struct {
    client *timescale.Client
}

func (c *DatabaseChecker) Name() string {
    return "timescaledb"
}

func (c *DatabaseChecker) IsCritical() bool {
    return true
}

func (c *DatabaseChecker) Check(ctx context.Context) error {
    return c.client.Ping(ctx)
}

// # RedisChecker implements health check for Redis
type RedisChecker struct {
    client *redis.Client
}

func (c *RedisChecker) Name() string {
    return "redis"
}

func (c *RedisChecker) IsCritical() bool {
    return false // # Redis is optional (caching)
}

func (c *RedisChecker) Check(ctx context.Context) error {
    return c.client.Ping(ctx).Err()
}

// # S3Checker implements health check for S3 access
type S3Checker struct {
    bucketName string
}

func (c *S3Checker) Name() string {
    return "s3-model-storage"
}

func (c *S3Checker) IsCritical() bool {
    return false
}

func (c *S3Checker) Check(ctx context.Context) error {
    // # Check S3 bucket accessibility
    _, err := s3Client.HeadBucket(ctx, &s3.HeadBucketInput{
        Bucket: aws.String(c.bucketName),
    })
    return err
}

// # collectSystemInfo gathers runtime system information
func collectSystemInfo() SystemInfo {
    var m runtime.MemStats
    runtime.ReadMemStats(&m)
    
    return SystemInfo{
        GoVersion:     runtime.Version(),
        NumGoroutine:  runtime.NumGoroutine(),
        NumCPU:        runtime.NumCPU(),
        MemAlloc:      m.Alloc / 1024 / 1024,
        MemTotalAlloc: m.TotalAlloc / 1024 / 1024,
    }
}

// # WriteJSON helper for consistent JSON responses
func WriteJSON(w http.ResponseWriter, statusCode int, data interface{}) {
    w.Header().Set("Content-Type", "application/json; charset=utf-8")
    w.WriteHeader(statusCode)
    
    if err := json.NewEncoder(w).Encode(data); err != nil {
        log.Error().Err(err).Msg("Failed to encode JSON response")
    }
}

// # WriteError helper for consistent error responses
func WriteError(w http.ResponseWriter, statusCode int, errorType string, message string) {
    w.Header().Set("Content-Type", "application/json; charset=utf-8")
    w.WriteHeader(statusCode)
    
    json.NewEncoder(w).Encode(map[string]interface{}{
        "error":   errorType,
        "message": message,
        "status":  statusCode,
        "timestamp": time.Now().Format(time.RFC3339),
    })
}
