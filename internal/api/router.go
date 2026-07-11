package api

import (
    "net/http"
    "time"

    "github.com/go-chi/chi/v5"
    "github.com/go-chi/chi/v5/middleware"
    "github.com/go-chi/cors"
    "github.com/go-chi/httprate"
    "github.com/riandyrn/otelchi"

    "resource-forecaster/internal/api/handlers"
    "resource-forecaster/internal/api/middleware/custommiddleware"
    "resource-forecaster/internal/config"
    "resource-forecaster/internal/forecaster"
    "resource-forecaster/internal/storage/timescale"
)

// # NewRouter creates the main API router with all middleware and routes
// # Implements clean architecture with proper separation of concerns
func NewRouter(
    tsClient *timescale.Client,
    forecastingEngine *forecaster.Engine,
    authCfg config.AuthConfig,
    rateLimitCfg middleware.RateLimitConfig,
) http.Handler {
    r := chi.NewRouter()

    // # Initialize core middleware components
    authMiddleware := middleware.NewAuthMiddleware(authCfg)
    tracingMiddleware := middleware.NewTracingMiddleware("resource-forecaster")
    rateLimiter := middleware.NewRateLimiter(redisClient, rateLimitCfg)
    loggingMiddleware := middleware.NewStructuredLogger()

    // # Initialize handlers with dependencies
    healthHandler := handlers.NewHealthHandler(tsClient)
    metricsHandler := handlers.NewMetricsHandler(tsClient)
    forecastHandler := handlers.NewForecastHandler(forecastingEngine, tsClient)
    adminHandler := handlers.NewAdminHandler(forecastingEngine, tsClient)

    // # Global middleware chain (applied to all routes)
    r.Use(middleware.RequestID)
    r.Use(middleware.RealIP)
    r.Use(loggingMiddleware.Logger)
    r.Use(tracingMiddleware.Trace)
    r.Use(middleware.Recoverer)
    r.Use(rateLimiter.RateLimit)
    r.Use(otelchi.Middleware("resource-forecaster",
        otelchi.WithChiRoutes(r),
    ))

    // # CORS configuration for production
    r.Use(cors.Handler(cors.Options{
        AllowedOrigins:   []string{"https://*.company.com", "https://dashboard.internal"},
        AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
        AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token", "X-Request-ID"},
        ExposedHeaders:   []string{"Link", "X-Request-ID", "X-Trace-ID", "X-RateLimit-*"},
        AllowCredentials: true,
        MaxAge:           300, // # Cache preflight for 5 minutes
    }))

    // # Security headers
    r.Use(middleware.SetHeader("X-Content-Type-Options", "nosniff"))
    r.Use(middleware.SetHeader("X-Frame-Options", "DENY"))
    r.Use(middleware.SetHeader("X-XSS-Protection", "1; mode=block"))
    r.Use(middleware.SetHeader("Referrer-Policy", "strict-origin-when-cross-origin"))
    r.Use(middleware.SetHeader("Permissions-Policy", "geolocation=(), microphone=(), camera=()"))

    // # Timeout for all requests
    r.Use(middleware.Timeout(30 * time.Second))

    // # API v1 routes
    r.Route("/api/v1", func(r chi.Router) {
        // # Public routes (no auth required)
        r.Group(func(r chi.Router) {
            r.Get("/health/live", healthHandler.LivenessCheck)
            r.Get("/health/ready", healthHandler.ReadinessCheck)
            r.Get("/health/startup", healthHandler.StartupCheck)
        })

        // # Semi-public routes (API key or JWT)
        r.Group(func(r chi.Router) {
            r.Use(authMiddleware.Authenticate)
            
            // # Metrics ingestion endpoint
            r.Post("/metrics", metricsHandler.IngestMetrics)
            r.Post("/metrics/batch", metricsHandler.BatchIngestMetrics)
            r.Get("/metrics/{hostname}", metricsHandler.GetMetrics)
        })

        // # Authenticated routes (JWT required)
        r.Group(func(r chi.Router) {
            r.Use(authMiddleware.Authenticate)
            r.Use(authMiddleware.RequireRole("analyst", "admin", "operator"))
            
            // # Forecasting endpoints
            r.Post("/forecast", forecastHandler.GenerateForecast)
            r.Get("/forecast/history/{hostname}", forecastHandler.GetForecastHistory)
            r.Get("/forecast/accuracy/{hostname}", forecastHandler.CompareForecastWithActual)
            r.Get("/forecast/recommendations/{hostname}", forecastHandler.GetRecommendations)
            
            // # Batch forecasting for multiple hosts
            r.Post("/forecast/batch", forecastHandler.BatchGenerateForecast)
            
            // # Anomaly detection
            r.Get("/anomalies/{hostname}", forecastHandler.DetectAnomalies)
        })

        // # Admin routes (admin role required)
        r.Group(func(r chi.Router) {
            r.Use(authMiddleware.Authenticate)
            r.Use(authMiddleware.RequireRole("admin"))
            
            // # Model management
            r.Post("/admin/models/train", adminHandler.TriggerTraining)
            r.Post("/admin/models/evaluate", adminHandler.TriggerEvaluation)
            r.Get("/admin/models/status", adminHandler.GetModelStatus)
            r.Put("/admin/models/weights", adminHandler.UpdateModelWeights)
            
            // # System configuration
            r.Get("/admin/config", adminHandler.GetConfig)
            r.Put("/admin/config", adminHandler.UpdateConfig)
            r.Post("/admin/cache/clear", adminHandler.ClearCache)
            
            // # Data management
            r.Post("/admin/data/retention", adminHandler.EnforceRetention)
            r.Post("/admin/data/backup", adminHandler.TriggerBackup)
            r.Get("/admin/data/stats", adminHandler.GetDataStats)
        })

        // # Service account routes (API key only)
        r.Group(func(r chi.Router) {
            r.Use(authMiddleware.ValidateServiceAccount)
            
            // # Automated metric ingestion
            r.Post("/collector/metrics", metricsHandler.IngestMetrics)
            
            // # Webhook for cloud provider alerts
            r.Post("/webhooks/cloudwatch", handlers.CloudWatchWebhook)
            r.Post("/webhooks/azure-monitor", handlers.AzureMonitorWebhook)
        })
    })

    // # Metrics endpoint for Prometheus scraping
    r.Handle("/metrics", promhttp.HandlerFor(
        prometheus.DefaultGatherer,
        promhttp.HandlerOpts{
            EnableOpenMetrics: true,
            Registry:          prometheus.DefaultRegisterer,
        },
    ))

    // # API documentation
    r.Handle("/docs/*", httpSwagger.Handler(
        httpSwagger.URL("/docs/swagger.json"),
    ))

    // # 404 handler for undefined routes
    r.NotFound(func(w http.ResponseWriter, r *http.Request) {
        handlers.WriteError(w, http.StatusNotFound, "not_found", 
            "The requested resource was not found")
    })

    // # 405 handler for unsupported methods
    r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
        handlers.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed",
            "The requested method is not supported for this resource")
    })

    return r
}

// # MountRoutes adds routes to an existing router (for microservices composition)
func MountRoutes(r chi.Router, deps Dependencies) {
    r.Mount("/forecaster", NewRouter(
        deps.TimescaleClient,
        deps.ForecastingEngine,
        deps.AuthConfig,
        deps.RateLimitConfig,
    ))
}
