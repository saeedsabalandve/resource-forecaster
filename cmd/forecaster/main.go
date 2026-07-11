package main

import (
    "context"
    "fmt"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/rs/zerolog"
    "github.com/rs/zerolog/log"
    "go.uber.org/automaxprocs/maxprocs"

    "resource-forecaster/internal/api"
    "resource-forecaster/internal/collector"
    "resource-forecaster/internal/config"
    "resource-forecaster/internal/forecaster"
    "resource-forecaster/internal/scheduler"
    "resource-forecaster/internal/storage/timescale"
    "resource-forecaster/internal/telemetry"
)

// # Bootstrap function initializes all core dependencies with proper shutdown ordering
// # Implements graceful degradation pattern for non-critical components
func main() {
    // # Initialize structured JSON logger with service context
    zerolog.TimeFieldFormat = time.RFC3339Nano
    zerolog.SetGlobalLevel(zerolog.InfoLevel)
    
    // # Auto-configure GOMAXPROCS for containerized environments (cgroup-aware)
    undo, err := maxprocs.Set(maxprocs.Logger(log.Printf))
    defer undo()
    if err != nil {
        log.Warn().Err(err).Msg("Failed to set GOMAXPROCS automatically")
    }

    // # Load configuration with hot-reload capability
    cfg, err := config.Load()
    if err != nil {
        log.Fatal().Err(err).Msg("Failed to load configuration")
    }

    // # Initialize OpenTelemetry tracing with AWS X-Ray/Azure Monitor exporters
    tp, err := telemetry.InitTracer(cfg.Telemetry)
    if err != nil {
        log.Fatal().Err(err).Msg("Failed to initialize tracer")
    }
    defer tp.Shutdown(context.Background())

    // # Initialize Prometheus metrics registry with custom collectors
    metricsServer := telemetry.InitMetrics(cfg.Telemetry)

    // # Establish TimescaleDB connection pool with connection retry logic
    tsClient, err := timescale.NewClient(cfg.TimescaleDB, cfg.Telemetry)
    if err != nil {
        log.Fatal().Err(err).Msg("Failed to connect to TimescaleDB")
    }
    defer tsClient.Close()

    // # Run database migrations with versioning
    if err := tsClient.RunMigrations(); err != nil {
        log.Fatal().Err(err).Msg("Failed to run database migrations")
    }

    // # Initialize metric collector with cloud-specific gatherers
    metricCollector := collector.NewCollector(
        tsClient,
        collector.WithGatherers(
            collector.NewCPUGatherer(),
            collector.NewMemoryGatherer(),
            collector.NewDiskGatherer(),
            collector.NewNetworkGatherer(),
            collector.NewGPUGatherer(), // # Optional GPU telemetry for ML workloads
        ),
    )

    // # Initialize forecasting engine with ensemble models
    forecastingEngine := forecaster.NewEngine(
        tsClient,
        forecaster.WithModels(
            forecaster.NewARIMAModel(),
            forecaster.NewProphetModel(),
            forecaster.NewLSTMModel(),
        ),
        forecaster.WithEnsembleStrategy("weighted_average"),
    )

    // # Setup scheduler for periodic metric collection and model retraining
    jobScheduler := scheduler.NewScheduler(
        metricCollector,
        forecastingEngine,
        cfg.Scheduler,
    )

    // # Start background jobs for metric collection and model training
    jobScheduler.Start()

    // # Initialize HTTP router with all middleware and handlers
    router := api.NewRouter(
        tsClient,
        forecastingEngine,
        cfg.Auth,
    )

    // # Create HTTP server with proper timeouts and keep-alive settings
    srv := &http.Server{
        Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
        Handler:      router,
        ReadTimeout:  15 * time.Second,
        WriteTimeout: 30 * time.Second,
        IdleTimeout:  60 * time.Second,
        MaxHeaderBytes: 1 << 20, // # 1MB max header size for security
    }

    // # Start metrics server on separate port (Prometheus scraping)
    go func() {
        log.Info().Msgf("Starting metrics server on :%d", cfg.Telemetry.MetricsPort)
        if err := metricsServer.ListenAndServe(); err != nil {
            log.Fatal().Err(err).Msg("Metrics server failed")
        }
    }()

    // # Start main API server in goroutine
    go func() {
        log.Info().Msgf("Starting API server on :%d", cfg.Server.Port)
        if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            log.Fatal().Err(err).Msg("API server failed")
        }
    }()

    // # Graceful shutdown with proper cleanup ordering
    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
    <-quit

    log.Info().Msg("Shutting down gracefully...")

    // # Stop accepting new requests first
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    // # Shutdown HTTP server
    if err := srv.Shutdown(ctx); err != nil {
        log.Error().Err(err).Msg("HTTP server forced to shutdown")
    }

    // # Stop scheduler and flush pending jobs
    jobScheduler.Stop()

    // # Close database connections
    tsClient.Close()

    log.Info().Msg("Server exited cleanly")
}
