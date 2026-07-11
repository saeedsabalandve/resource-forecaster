package scheduler

import (
    "context"
    "fmt"
    "time"

    "github.com/robfig/cron/v3"
    "github.com/rs/zerolog/log"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/codes"
    "go.opentelemetry.io/otel/metric"

    "resource-forecaster/internal/collector"
    "resource-forecaster/internal/forecaster"
    "resource-forecaster/internal/storage/timescale"
)

// # JobScheduler orchestrates periodic tasks with production-grade reliability
type JobScheduler struct {
    cron          *cron.Cron
    collector     *collector.Collector
    forecaster    *forecaster.Engine
    storage       *timescale.Client
    
    // # Job configurations
    collectionInterval    time.Duration
    trainingInterval      time.Duration
    evaluationInterval    time.Duration
    forecastHorizonHours  int
    retentionPeriodDays   int
    
    // # Metrics for job monitoring
    collectionJobsTotal    metric.Int64Counter
    trainingJobsTotal      metric.Int64Counter
    evaluationJobsTotal    metric.Int64Counter
    jobDuration            metric.Float64Histogram
    activeJobs             metric.Int64UpDownCounter
    
    // # Error tracking
    consecutiveFailures map[string]int
    maxRetries         int
    retryDelay         time.Duration
}

// # NewScheduler creates production scheduler with cron and monitoring
func NewScheduler(
    collector *collector.Collector,
    forecaster *forecaster.Engine,
    storage *timescale.Client,
    collectionInterval time.Duration,
    trainingInterval time.Duration,
    evaluationInterval time.Duration,
) *JobScheduler {
    meter := otel.Meter("job-scheduler")
    
    collectionJobsTotal, _ := meter.Int64Counter(
        "scheduler_collection_jobs_total",
        metric.WithDescription("Total number of metric collection jobs executed"),
    )
    
    trainingJobsTotal, _ := meter.Int64Counter(
        "scheduler_training_jobs_total",
        metric.WithDescription("Total number of model training jobs executed"),
    )
    
    evaluationJobsTotal, _ := meter.Int64Counter(
        "scheduler_evaluation_jobs_total",
        metric.WithDescription("Total number of model evaluation jobs executed"),
    )
    
    jobDuration, _ := meter.Float64Histogram(
        "scheduler_job_duration_seconds",
        metric.WithDescription("Duration of scheduled jobs"),
        metric.WithExplicitBucketBoundaries(1, 5, 10, 30, 60, 120, 300, 600),
    )
    
    activeJobs, _ := meter.Int64UpDownCounter(
        "scheduler_active_jobs",
        metric.WithDescription("Number of currently active scheduled jobs"),
    )
    
    return &JobScheduler{
        cron:                  cron.New(cron.WithSeconds(), cron.WithLocation(time.UTC)),
        collector:             collector,
        forecaster:            forecaster,
        storage:               storage,
        collectionInterval:    collectionInterval,
        trainingInterval:      trainingInterval,
        evaluationInterval:    evaluationInterval,
        forecastHorizonHours:  168, // # 1 week default
        retentionPeriodDays:   365, // # 1 year default
        collectionJobsTotal:   collectionJobsTotal,
        trainingJobsTotal:     trainingJobsTotal,
        evaluationJobsTotal:   evaluationJobsTotal,
        jobDuration:           jobDuration,
        activeJobs:            activeJobs,
        consecutiveFailures:   make(map[string]int),
        maxRetries:            3,
        retryDelay:            30 * time.Second,
    }
}

// # Start begins all scheduled jobs
func (s *JobScheduler) Start() {
    ctx := context.Background()
    
    // # 1. Metric Collection Job - runs every collection interval
    collectionCronExpr := fmt.Sprintf("@every %s", s.collectionInterval.String())
    _, err := s.cron.AddFunc(collectionCronExpr, func() {
        s.executeWithRetry(ctx, "metric_collection", s.collectMetrics)
    })
    if err != nil {
        log.Fatal().Err(err).Msg("Failed to schedule metric collection job")
    }
    
    // # 2. Model Training Job - runs less frequently
    trainingCronExpr := fmt.Sprintf("@every %s", s.trainingInterval.String())
    _, err = s.cron.AddFunc(trainingCronExpr, func() {
        s.executeWithRetry(ctx, "model_training", s.trainModels)
    })
    if err != nil {
        log.Fatal().Err(err).Msg("Failed to schedule model training job")
    }
    
    // # 3. Model Evaluation Job - runs daily
    evaluationCronExpr := fmt.Sprintf("@every %s", s.evaluationInterval.String())
    _, err = s.cron.AddFunc(evaluationCronExpr, func() {
        s.executeWithRetry(ctx, "model_evaluation", s.evaluateModels)
    })
    if err != nil {
        log.Fatal().Err(err).Msg("Failed to schedule model evaluation job")
    }
    
    // # 4. Data Retention Job - runs daily at midnight
    _, err = s.cron.AddFunc("0 0 0 * * *", func() {
        s.executeWithRetry(ctx, "data_retention", s.enforceDataRetention)
    })
    if err != nil {
        log.Fatal().Err(err).Msg("Failed to schedule data retention job")
    }
    
    // # 5. Health Check Job - runs every 5 minutes
    _, err = s.cron.AddFunc("0 */5 * * * *", func() {
        s.healthCheck(ctx)
    })
    if err != nil {
        log.Fatal().Err(err).Msg("Failed to schedule health check job")
    }
    
    s.cron.Start()
    
    log.Info().
        Dur("collection_interval", s.collectionInterval).
        Dur("training_interval", s.trainingInterval).
        Dur("evaluation_interval", s.evaluationInterval).
        Msg("Job scheduler started with all periodic tasks")
}

// # Stop gracefully shuts down the scheduler
func (s *JobScheduler) Stop() {
    log.Info().Msg("Stopping job scheduler...")
    
    ctx := s.cron.Stop()
    <-ctx.Done()
    
    log.Info().Msg("Job scheduler stopped")
}

// # executeWithRetry wraps job execution with retry logic and observability
func (s *JobScheduler) executeWithRetry(ctx context.Context, jobName string, jobFunc func(context.Context) error) {
    tracer := otel.Tracer("job-scheduler")
    ctx, span := tracer.Start(ctx, fmt.Sprintf("job-%s", jobName))
    defer span.End()
    
    span.SetAttributes(
        attribute.String("job.name", jobName),
        attribute.String("job.start_time", time.Now().Format(time.RFC3339)),
    )
    
    s.activeJobs.Add(ctx, 1)
    defer s.activeJobs.Add(ctx, -1)
    
    startTime := time.Now()
    
    var lastErr error
    for attempt := 1; attempt <= s.maxRetries; attempt++ {
        err := jobFunc(ctx)
        if err == nil {
            // # Job succeeded
            s.consecutiveFailures[jobName] = 0
            s.incrementJobCounter(jobName)
            
            duration := time.Since(startTime)
            s.jobDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(
                attribute.String("job", jobName),
                attribute.String("status", "success"),
            ))
            
            span.SetStatus(codes.Ok, "Job completed successfully")
            span.SetAttributes(
                attribute.Int("attempt", attempt),
                attribute.Float64("duration_seconds", duration.Seconds()),
            )
            
            log.Debug().
                Str("job", jobName).
                Int("attempt", attempt).
                Dur("duration", duration).
                Msg("Job completed successfully")
            
            return
        }
        
        lastErr = err
        s.consecutiveFailures[jobName]++
        
        log.Warn().
            Err(err).
            Str("job", jobName).
            Int("attempt", attempt).
            Int("max_retries", s.maxRetries).
            Msg("Job failed, retrying...")
        
        span.SetAttributes(
            attribute.Int("failed_attempt", attempt),
            attribute.String("error", err.Error()),
        )
        
        if attempt < s.maxRetries {
            // # Exponential backoff
            backoffDuration := s.retryDelay * time.Duration(1<<uint(attempt-1))
            time.Sleep(backoffDuration)
        }
    }
    
    // # All retries exhausted
    duration := time.Since(startTime)
    s.jobDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(
        attribute.String("job", jobName),
        attribute.String("status", "failed"),
    ))
    
    span.RecordError(lastErr)
    span.SetStatus(codes.Error, fmt.Sprintf("Job failed after %d attempts", s.maxRetries))
    
    // # Alert if too many consecutive failures
    if s.consecutiveFailures[jobName] >= s.maxRetries*3 {
        log.Error().
            Err(lastErr).
            Str("job", jobName).
            Int("consecutive_failures", s.consecutiveFailures[jobName]).
            Msg("ALERT: Job has been failing consistently, requires manual intervention")
    }
}

// # collectMetrics gathers and stores system resource metrics
func (s *JobScheduler) collectMetrics(ctx context.Context) error {
    ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
    defer cancel()
    
    log.Debug().Msg("Starting metric collection job")
    
    if err := s.collector.Collect(ctx); err != nil {
        return fmt.Errorf("metric collection failed: %w", err)
    }
    
    log.Debug().Msg("Metric collection completed")
    return nil
}

// # trainModels triggers model retraining with latest data
func (s *JobScheduler) trainModels(ctx context.Context) error {
    ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
    defer cancel()
    
    log.Info().Msg("Starting model training job")
    
    // # Get all hostnames that have recent data
    hostnames, err := s.storage.GetActiveHostnames(ctx, 24*time.Hour)
    if err != nil {
        return fmt.Errorf("failed to get active hostnames: %w", err)
    }
    
    if len(hostnames) == 0 {
        log.Info().Msg("No active hostnames found for training")
        return nil
    }
    
    log.Info().Int("hostname_count", len(hostnames)).Msg("Training models for hostnames")
    
    // # Train models for each hostname and metric type
    metricTypes := []string{"cpu", "memory", "disk", "network"}
    
    for _, hostname := range hostnames {
        for _, metricType := range metricTypes {
            // # Get historical data
            endTime := time.Now()
            startTime := endTime.Add(-30 * 24 * time.Hour) // # 30 days of training data
            
            timestamps, values, err := s.storage.GetHistoricalMetrics(
                ctx, hostname, metricType, startTime, endTime,
            )
            if err != nil {
                log.Warn().
                    Err(err).
                    Str("hostname", hostname).
                    Str("metric_type", metricType).
                    Msg("Failed to get training data")
                continue
            }
            
            if len(timestamps) < 24 {
                log.Debug().
                    Str("hostname", hostname).
                    Str("metric_type", metricType).
                    Int("data_points", len(timestamps)).
                    Msg("Insufficient data for training")
                continue
            }
            
            // # Train models
            if err := s.forecaster.TrainModels(ctx, hostname, metricType, timestamps, values); err != nil {
                log.Error().
                    Err(err).
                    Str("hostname", hostname).
                    Str("metric_type", metricType).
                    Msg("Model training failed")
                continue
            }
            
            log.Debug().
                Str("hostname", hostname).
                Str("metric_type", metricType).
                Int("data_points", len(timestamps)).
                Msg("Model training completed")
        }
    }
    
    log.Info().Msg("Model training job completed")
    return nil
}

// # evaluateModels assesses model accuracy against actual values
func (s *JobScheduler) evaluateModels(ctx context.Context) error {
    ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
    defer cancel()
    
    log.Info().Msg("Starting model evaluation job")
    
    // # Get recent forecasts and compare with actual metrics
    hostnames, err := s.storage.GetActiveHostnames(ctx, 7*24*time.Hour)
    if err != nil {
        return fmt.Errorf("failed to get active hostnames: %w", err)
    }
    
    for _, hostname := range hostnames {
        for _, metricType := range []string{"cpu", "memory"} {
            accuracy, err := s.forecaster.EvaluateForecastAccuracy(ctx, hostname, metricType)
            if err != nil {
                log.Warn().
                    Err(err).
                    Str("hostname", hostname).
                    Str("metric_type", metricType).
                    Msg("Model evaluation failed")
                continue
            }
            
            log.Info().
                Str("hostname", hostname).
                Str("metric_type", metricType).
                Interface("accuracy", accuracy).
                Msg("Model evaluation completed")
        }
    }
    
    log.Info().Msg("Model evaluation job completed")
    return nil
}

// # enforceDataRetention removes data older than retention period
func (s *JobScheduler) enforceDataRetention(ctx context.Context) error {
    ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
    defer cancel()
    
    log.Info().Int("retention_days", s.retentionPeriodDays).Msg("Starting data retention enforcement")
    
    if err := s.storage.EnforceRetentionPolicy(ctx, s.retentionPeriodDays); err != nil {
        return fmt.Errorf("retention policy enforcement failed: %w", err)
    }
    
    log.Info().Msg("Data retention enforcement completed")
    return nil
}

// # healthCheck verifies all components are functioning
func (s *JobScheduler) healthCheck(ctx context.Context) {
    // # Check database connectivity
    if err := s.storage.Ping(ctx); err != nil {
        log.Error().Err(err).Msg("Health check failed: Database unreachable")
    }
    
    // # Check if collector is functioning
    // # Check if models are up to date
    // # Report metrics about scheduler health
}
