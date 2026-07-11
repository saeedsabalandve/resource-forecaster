package middleware

import (
    "context"
    "fmt"
    "net/http"
    "sync"
    "time"

    "github.com/redis/go-redis/v9"
    "github.com/rs/zerolog/log"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/metric"
    "golang.org/x/time/rate"
)

// # RateLimiter implements multi-tier rate limiting:
// # Tier 1: Per-IP token bucket (local, fast)
// # Tier 2: Per-user sliding window (Redis, distributed)
// # Tier 3: Global rate limit (Redis, cluster-wide)
type RateLimiter struct {
    redisClient     *redis.Client
    
    // # Local rate limiters (in-memory, per-IP)
    localLimiters    map[string]*rateLimiterEntry
    localMu          sync.RWMutex
    localRate        rate.Limit
    localBurst       int
    localCleanupTick time.Duration
    
    // # Redis rate limit keys
    redisKeyPrefix   string
    
    // # Rate limit configurations
    globalRPS        int     // # Global requests per second
    perUserRPS       int     // # Per-user requests per second
    perIPRPS         int     // # Per-IP requests per second
    
    // # Metrics
    rateLimitedCounter metric.Int64Counter
}

type rateLimiterEntry struct {
    limiter  *rate.Limiter
    lastUsed time.Time
}

// # RateLimitConfig defines rate limiting parameters
type RateLimitConfig struct {
    GlobalRPS        int           // # Global requests per second across all instances
    PerUserRPS       int           // # Per-user/API key requests per second
    PerIPRPS         int           // # Per-IP requests per second  
    LocalBurst       int           // # Burst size for local token bucket
    CleanupInterval  time.Duration // # Local limiter cleanup interval
    RedisKeyPrefix   string        // # Redis key prefix for rate limit keys
}

// # NewRateLimiter creates a production-grade rate limiter
func NewRateLimiter(redisClient *redis.Client, config RateLimitConfig) *RateLimiter {
    meter := otel.Meter("rate-limiter")
    
    rateLimitedCounter, _ := meter.Int64Counter(
        "rate_limited_requests_total",
        metric.WithDescription("Total number of rate-limited requests"),
        metric.WithUnit("{request}"),
    )
    
    rl := &RateLimiter{
        redisClient:       redisClient,
        localLimiters:     make(map[string]*rateLimiterEntry),
        localRate:         rate.Limit(config.PerIPRPS),
        localBurst:        config.LocalBurst,
        localCleanupTick:  config.CleanupInterval,
        redisKeyPrefix:    config.RedisKeyPrefix,
        globalRPS:         config.GlobalRPS,
        perUserRPS:        config.PerUserRPS,
        perIPRPS:          config.PerIPRPS,
        rateLimitedCounter: rateLimitedCounter,
    }
    
    // # Start background cleanup of stale local limiters
    go rl.cleanupLoop()
    
    return rl
}

// # RateLimit middleware checks rate limits in order: local -> per-user -> global
func (rl *RateLimiter) RateLimit(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        ctx := r.Context()
        
        // # Extract client identifier
        clientIP := extractClientIP(r)
        userID := extractUserID(r)
        
        // # Tier 1: Local per-IP rate limit (fastest)
        if !rl.checkLocalLimit(clientIP) {
            rl.rateLimitExceeded(w, r, "local_ip_limit")
            return
        }
        
        // # Tier 2: Per-user rate limit (distributed)
        if userID != "" {
            if !rl.checkRedisRateLimit(ctx, fmt.Sprintf("user:%s", userID), rl.perUserRPS) {
                rl.rateLimitExceeded(w, r, "user_limit")
                return
            }
        }
        
        // # Tier 3: Global rate limit (cluster-wide)
        if !rl.checkRedisRateLimit(ctx, "global", rl.globalRPS) {
            rl.rateLimitExceeded(w, r, "global_limit")
            return
        }
        
        // # Add rate limit headers for client awareness
        w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%d", rl.perUserRPS))
        w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", rl.getRemainingTokens(clientIP)))
        w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(time.Second).Unix()))
        
        next.ServeHTTP(w, r)
    })
}

// # checkLocalLimit implements token bucket algorithm for per-IP limiting
func (rl *RateLimiter) checkLocalLimit(clientIP string) bool {
    rl.localMu.RLock()
    entry, exists := rl.localLimiters[clientIP]
    rl.localMu.RUnlock()
    
    if !exists {
        // # Create new limiter for this IP
        rl.localMu.Lock()
        // # Double-check after acquiring write lock
        entry, exists = rl.localLimiters[clientIP]
        if !exists {
            entry = &rateLimiterEntry{
                limiter:  rate.NewLimiter(rl.localRate, rl.localBurst),
                lastUsed: time.Now(),
            }
            rl.localLimiters[clientIP] = entry
        }
        rl.localMu.Unlock()
    }
    
    // # Update last used timestamp
    entry.lastUsed = time.Now()
    
    // # Check if request is allowed by token bucket
    return entry.limiter.Allow()
}

// # checkRedisRateLimit implements sliding window rate limiting in Redis
func (rl *RateLimiter) checkRedisRateLimit(ctx context.Context, key string, limit int) bool {
    now := time.Now()
    windowStart := now.Add(-time.Second)
    
    redisKey := fmt.Sprintf("%s:%s", rl.redisKeyPrefix, key)
    
    // # Use Redis pipeline for atomic operations
    pipe := rl.redisClient.Pipeline()
    
    // # Remove expired entries (sliding window)
    pipe.ZRemRangeByScore(ctx, redisKey, "0", fmt.Sprintf("%d", windowStart.UnixNano()/1e6))
    
    // # Count current window entries
    countCmd := pipe.ZCard(ctx, redisKey)
    
    // # Add current request timestamp
    pipe.ZAdd(ctx, redisKey, redis.Z{
        Score:  float64(now.UnixNano() / 1e6),
        Member: fmt.Sprintf("%d", now.UnixNano()),
    })
    
    // # Set expiry on the key to prevent memory leaks
    pipe.Expire(ctx, redisKey, 2*time.Second)
    
    _, err := pipe.Exec(ctx)
    if err != nil {
        log.Error().Err(err).Str("key", key).Msg("Redis rate limit check failed")
        // # Fail open if Redis is unavailable (prefer availability over rate limiting)
        return true
    }
    
    currentCount := countCmd.Val()
    return currentCount < int64(limit)
}

// # getRemainingTokens returns remaining tokens for rate limit header
func (rl *RateLimiter) getRemainingTokens(clientIP string) int {
    rl.localMu.RLock()
    defer rl.localMu.RUnlock()
    
    if entry, exists := rl.localLimiters[clientIP]; exists {
        tokens := entry.limiter.Tokens()
        return int(tokens)
    }
    return rl.localBurst
}

// # rateLimitExceeded handles rate limit responses with proper headers
func (rl *RateLimiter) rateLimitExceeded(w http.ResponseWriter, r *http.Request, reason string) {
    tracer := otel.Tracer("rate-limiter")
    _, span := tracer.Start(r.Context(), "rate_limit_exceeded")
    defer span.End()
    
    span.SetAttributes(
        attribute.String("rate_limit_reason", reason),
        attribute.String("client_ip", extractClientIP(r)),
    )
    
    rl.rateLimitedCounter.Add(r.Context(), 1,
        metric.WithAttributes(attribute.String("reason", reason)),
    )
    
    w.Header().Set("Retry-After", "1")
    w.Header().Set("X-RateLimit-Reason", reason)
    
    http.Error(w, fmt.Sprintf(`{"error":"rate_limited","message":"Too many requests","retry_after":1}`), 
        http.StatusTooManyRequests)
    
    log.Warn().
        Str("client_ip", extractClientIP(r)).
        Str("path", r.URL.Path).
        Str("reason", reason).
        Msg("Rate limit exceeded")
}

// # cleanupLoop periodically removes stale local rate limiters
func (rl *RateLimiter) cleanupLoop() {
    ticker := time.NewTicker(rl.localCleanupTick)
    defer ticker.Stop()
    
    for range ticker.C {
        rl.localMu.Lock()
        now := time.Now()
        staleTimeout := 15 * time.Minute
        
        for ip, entry := range rl.localLimiters {
            if now.Sub(entry.lastUsed) > staleTimeout {
                delete(rl.localLimiters, ip)
            }
        }
        
        count := len(rl.localLimiters)
        rl.localMu.Unlock()
        
        if count > 0 {
            log.Debug().Int("active_limiters", count).Msg("Cleaned up stale rate limiters")
        }
    }
}

// # extractClientIP gets real client IP considering proxies
func extractClientIP(r *http.Request) string {
    // # Check X-Forwarded-For header (common with load balancers)
    if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
        ips := strings.Split(xff, ",")
        if len(ips) > 0 {
            return strings.TrimSpace(ips[0])
        }
    }
    
    // # Check X-Real-IP header
    if xri := r.Header.Get("X-Real-IP"); xri != "" {
        return xri
    }
    
    // # Fall back to remote address
    host, _, err := net.SplitHostPort(r.RemoteAddr)
    if err != nil {
        return r.RemoteAddr
    }
    return host
}

// # extractUserID gets authenticated user ID from context
func extractUserID(r *http.Request) string {
    if claims, ok := r.Context().Value(ContextKeyClaims).(*CustomClaims); ok {
        return claims.Subject
    }
    return ""
}
