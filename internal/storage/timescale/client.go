package timescale

import (
    "context"
    "fmt"
    "time"

    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgconn"
    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/rs/zerolog/log"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    semconv "go.opentelemetry.io/otel/semconv/v1.17.0"

    "resource-forecaster/internal/config"
    "resource-forecaster/internal/collector"
)

// # Client wraps TimescaleDB connection pool with production-grade configuration
// # Uses pgx v5 for native PostgreSQL wire protocol with TimescaleDB extensions
type Client struct {
    pool         *pgxpool.Pool
    config       config.TimescaleDBConfig
    isConnected  bool
}

// # NewClient creates a production-configured TimescaleDB client
func NewClient(cfg config.TimescaleDBConfig, telemetryCfg config.TelemetryConfig) (*Client, error) {
    // # Build connection string with SSL and connection pooling parameters
    connString := fmt.Sprintf(
        "postgres://%s:%s@%s:%d/%s?sslmode=%s&pool_max_conns=%d&pool_min_conns=%d&pool_max_conn_lifetime=%s&application_name=%s",
        cfg.Username,
        cfg.Password,
        cfg.Host,
        cfg.Port,
        cfg.Database,
        cfg.SSLMode,
        cfg.MaxConnections,
        cfg.MinConnections,
        cfg.MaxConnLifetime.String(),
        telemetryCfg.ServiceName,
    )

    // # Parse connection pool configuration with sensible defaults
    poolConfig, err := pgxpool.ParseConfig(connString)
    if err != nil {
        return nil, fmt.Errorf("failed to parse connection string: %w", err)
    }

    // # Configure connection pool health checks and timeouts
    poolConfig.MaxConnLifetime = cfg.MaxConnLifetime
    poolConfig.MaxConnIdleTime = 10 * time.Minute
    poolConfig.HealthCheckPeriod = 30 * time.Second
    
    // # Set connection timeouts
    poolConfig.ConnConfig.ConnectTimeout = 5 * time.Second
    
    // # Enable statement logging in development
    if telemetryCfg.LogLevel == "debug" {
        poolConfig.ConnConfig.Tracer = &tracer{}
    }

    // # Create connection pool with retry logic
    var pool *pgxpool.Pool
    maxRetries := 5
    for i := 0; i < maxRetries; i++ {
        pool, err = pgxpool.NewWithConfig(context.Background(), poolConfig)
        if err == nil {
            // # Verify connection with ping
            ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
            err = pool.Ping(ctx)
            cancel()
            if err == nil {
                break
            }
            pool.Close()
        }
        
        if i < maxRetries-1 {
            retryDelay := time.Duration(math.Pow(2, float64(i))) * time.Second
            log.Warn().
                Err(err).
                Int("attempt", i+1).
                Dur("retry_delay", retryDelay).
                Msg("Failed to connect to TimescaleDB, retrying...")
            time.Sleep(retryDelay)
        }
    }
    
    if err != nil {
        return nil, fmt.Errorf("failed to connect to TimescaleDB after %d attempts: %w", maxRetries, err)
    }

    log.Info().
        Str("host", cfg.Host).
        Int("port", cfg.Port).
        Str("database", cfg.Database).
        Int("max_connections", cfg.MaxConnections).
        Msg("Successfully connected to TimescaleDB")

    return &Client{
        pool: pool,
        config: cfg,
        isConnected: true,
    }, nil
}

// # StoreMetrics inserts collected metrics with TimescaleDB hyperfunctions
func (c *Client) StoreMetrics(ctx context.Context, metrics *collector.ResourceMetrics) error {
    tracer := otel.Tracer("timescale-storage")
    ctx, span := tracer.Start(ctx, "store-metrics")
    defer span.End()

    span.SetAttributes(
        attribute.String("hostname", metrics.Hostname),
        attribute.String("instance_id", metrics.InstanceID),
    )

    // # Use transaction for atomic multi-table insert
    tx, err := c.pool.Begin(ctx)
    if err != nil {
        return fmt.Errorf("failed to begin transaction: %w", err)
    }
    defer tx.Rollback(ctx)

    // # Insert CPU metrics with hypertable
    cpuQuery := `
        INSERT INTO cpu_metrics (
            timestamp, hostname, instance_id, instance_type,
            usage_percent, user_percent, system_percent, 
            iowait_percent, steal_percent, num_cores, throttled_time
        ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
    `
    _, err = tx.Exec(ctx, cpuQuery,
        metrics.Timestamp,
        metrics.Hostname,
        metrics.InstanceID,
        metrics.InstanceType,
        metrics.CPU.UsagePercent,
        metrics.CPU.UserPercent,
        metrics.CPU.SystemPercent,
        metrics.CPU.IOWaitPercent,
        metrics.CPU.StealPercent,
        metrics.CPU.NumCores,
        metrics.CPU.ThrottledTime,
    )
    if err != nil {
        return fmt.Errorf("failed to insert CPU metrics: %w", err)
    }

    // # Insert Memory metrics
    memQuery := `
        INSERT INTO memory_metrics (
            timestamp, hostname, instance_id,
            total_bytes, used_bytes, available_bytes, used_percent,
            swap_total_bytes, swap_used_bytes, cached_bytes, buffers_bytes
        ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
    `
    _, err = tx.Exec(ctx, memQuery,
        metrics.Timestamp,
        metrics.Hostname,
        metrics.InstanceID,
        metrics.Memory.TotalBytes,
        metrics.Memory.UsedBytes,
        metrics.Memory.AvailableBytes,
        metrics.Memory.UsedPercent,
        metrics.Memory.SwapTotalBytes,
        metrics.Memory.SwapUsedBytes,
        metrics.Memory.CachedBytes,
        metrics.Memory.BuffersBytes,
    )
    if err != nil {
        return fmt.Errorf("failed to insert memory metrics: %w", err)
    }

    // # Insert Disk metrics (multiple mounts)
    for _, disk := range metrics.Disk {
        diskQuery := `
            INSERT INTO disk_metrics (
                timestamp, hostname, instance_id,
                mount_point, device, total_bytes, used_bytes,
                available_bytes, used_percent, read_bytes_per_sec,
                write_bytes_per_sec, iops
            ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
        `
        _, err = tx.Exec(ctx, diskQuery,
            metrics.Timestamp,
            metrics.Hostname,
            metrics.InstanceID,
            disk.MountPoint,
            disk.Device,
            disk.TotalBytes,
            disk.UsedBytes,
            disk.AvailableBytes,
            disk.UsedPercent,
            disk.ReadBytesPerSec,
            disk.WriteBytesPerSec,
            disk.IOPS,
        )
        if err != nil {
            return fmt.Errorf("failed to insert disk metrics for %s: %w", disk.MountPoint, err)
        }
    }

    // # Insert Network metrics
    netQuery := `
        INSERT INTO network_metrics (
            timestamp, hostname, instance_id,
            interface_name, received_bytes_per_sec,
            transmitted_bytes_per_sec, packets_dropped,
            error_count, tcp_connections
        ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
    `
    _, err = tx.Exec(ctx, netQuery,
        metrics.Timestamp,
        metrics.Hostname,
        metrics.InstanceID,
        metrics.Network.InterfaceName,
        metrics.Network.ReceivedBytesPerSec,
        metrics.Network.TransmittedBytesPerSec,
        metrics.Network.PacketsDropped,
        metrics.Network.ErrorCount,
        metrics.Network.TCPConnections,
    )
    if err != nil {
        return fmt.Errorf("failed to insert network metrics: %w", err)
    }

    // # Insert GPU metrics if available
    for _, gpu := range metrics.GPU {
        gpuQuery := `
            INSERT INTO gpu_metrics (
                timestamp, hostname, instance_id,
                gpu_index, name, utilization_percent,
                memory_total_bytes, memory_used_bytes,
                temperature_celsius, power_usage_watts
            ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
        `
        _, err = tx.Exec(ctx, gpuQuery,
            metrics.Timestamp,
            metrics.Hostname,
            metrics.InstanceID,
            gpu.Index,
            gpu.Name,
            gpu.UtilizationPercent,
            gpu.MemoryTotalBytes,
            gpu.MemoryUsedBytes,
            gpu.TemperatureCelsius,
            gpu.PowerUsageWatts,
        )
        if err != nil {
            log.Warn().Err(err).Int("gpu_index", gpu.Index).Msg("Failed to insert GPU metrics")
        }
    }

    // # Insert Load Average
    loadQuery := `
        INSERT INTO load_average_metrics (
            timestamp, hostname, instance_id,
            load1, load5, load15
        ) VALUES ($1, $2, $3, $4, $5, $6)
    `
    _, err = tx.Exec(ctx, loadQuery,
        metrics.Timestamp,
        metrics.Hostname,
        metrics.InstanceID,
        metrics.LoadAverage.Load1,
        metrics.LoadAverage.Load5,
        metrics.LoadAverage.Load15,
    )
    if err != nil {
        return fmt.Errorf("failed to insert load average metrics: %w", err)
    }

    // # Commit transaction
    if err := tx.Commit(ctx); err != nil {
        return fmt.Errorf("failed to commit transaction: %w", err)
    }

    span.SetAttributes(attribute.Int("metrics_inserted", 1))
    
    log.Debug().
        Str("hostname", metrics.Hostname).
        Time("timestamp", metrics.Timestamp).
        Msg("Metrics stored successfully")

    return nil
}

// # GetHistoricalMetrics retrieves time-series data for forecasting
func (c *Client) GetHistoricalMetrics(
    ctx context.Context, 
    hostname string, 
    metricType string,
    startTime, endTime time.Time,
) ([]time.Time, []float64, error) {
    tracer := otel.Tracer("timescale-storage")
    ctx, span := tracer.Start(ctx, "get-historical-metrics")
    defer span.End()

    span.SetAttributes(
        attribute.String("hostname", hostname),
        attribute.String("metric_type", metricType),
        attribute.String("time_range", endTime.Sub(startTime).String()),
    )

    var query string
    switch metricType {
    case "cpu":
        query = `
            SELECT timestamp, usage_percent 
            FROM cpu_metrics 
            WHERE hostname = $1 
                AND timestamp BETWEEN $2 AND $3
            ORDER BY timestamp ASC
        `
    case "memory":
        query = `
            SELECT timestamp, used_percent 
            FROM memory_metrics 
            WHERE hostname = $1 
                AND timestamp BETWEEN $2 AND $3
            ORDER BY timestamp ASC
        `
    default:
        return nil, nil, fmt.Errorf("unsupported metric type: %s", metricType)
    }

    rows, err := c.pool.Query(ctx, query, hostname, startTime, endTime)
    if err != nil {
        return nil, nil, fmt.Errorf("failed to query historical metrics: %w", err)
    }
    defer rows.Close()

    var timestamps []time.Time
    var values []float64

    for rows.Next() {
        var ts time.Time
        var val float64
        if err := rows.Scan(&ts, &val); err != nil {
            return nil, nil, fmt.Errorf("failed to scan row: %w", err)
        }
        timestamps = append(timestamps, ts)
        values = append(values, val)
    }

    if err := rows.Err(); err != nil {
        return nil, nil, fmt.Errorf("error iterating rows: %w", err)
    }

    return timestamps, values, nil
}

// # RunMigrations applies database schema migrations
func (c *Client) RunMigrations() error {
    ctx := context.Background()
    
    // # Create TimescaleDB extension if not exists
    _, err := c.pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS timescaledb CASCADE")
    if err != nil {
        return fmt.Errorf("failed to create timescaledb extension: %w", err)
    }

    // # Create CPU metrics hypertable with optimal chunking
    _, err = c.pool.Exec(ctx, `
        CREATE TABLE IF NOT EXISTS cpu_metrics (
            timestamp       TIMESTAMPTZ NOT NULL,
            hostname        TEXT NOT NULL,
            instance_id     TEXT,
            instance_type   TEXT,
            usage_percent   DOUBLE PRECISION,
            user_percent    DOUBLE PRECISION,
            system_percent  DOUBLE PRECISION,
            iowait_percent  DOUBLE PRECISION,
            steal_percent   DOUBLE PRECISION,
            num_cores       INTEGER,
            throttled_time  DOUBLE PRECISION,
            PRIMARY KEY (timestamp, hostname)
        )
    `)
    if err != nil {
        return err
    }

    // # Convert to hypertable with 1-hour chunks for optimal query performance
    _, err = c.pool.Exec(ctx, `
        SELECT create_hypertable('cpu_metrics', 'timestamp', 
            chunk_time_interval => INTERVAL '1 hour',
            if_not_exists => TRUE
        )
    `)
    if err != nil {
        return err
    }

    // # Create compression policy for old data (after 7 days)
    _, err = c.pool.Exec(ctx, `
        ALTER TABLE cpu_metrics SET (
            timescaledb.compress,
            timescaledb.compress_segmentby = 'hostname',
            timescaledb.compress_orderby = 'timestamp DESC'
        )
    `)
    if err != nil {
        log.Warn().Err(err).Msg("Failed to set compression on cpu_metrics")
    }

    // # Add compression policy
    _, err = c.pool.Exec(ctx, `
        SELECT add_compression_policy('cpu_metrics', INTERVAL '7 days', 
            if_not_exists => TRUE
        )
    `)
    if err != nil {
        log.Warn().Err(err).Msg("Failed to add compression policy")
    }

    // # Create retention policy for data older than retention period
    if c.config.RetentionDays > 0 {
        _, err = c.pool.Exec(ctx, fmt.Sprintf(`
            SELECT add_retention_policy('cpu_metrics', INTERVAL '%d days',
                if_not_exists => TRUE
            )
        `, c.config.RetentionDays))
        if err != nil {
            log.Warn().Err(err).Msg("Failed to add retention policy")
        }
    }

    // # Create indexes for common query patterns
    indexes := []string{
        `CREATE INDEX IF NOT EXISTS idx_cpu_metrics_timestamp 
            ON cpu_metrics (timestamp DESC)`,
        `CREATE INDEX IF NOT EXISTS idx_cpu_metrics_hostname_time 
            ON cpu_metrics (hostname, timestamp DESC)`,
        `CREATE INDEX IF NOT EXISTS idx_cpu_metrics_instance 
            ON cpu_metrics (instance_id, timestamp DESC)`,
    }

    for _, idx := range indexes {
        _, err = c.pool.Exec(ctx, idx)
        if err != nil {
            log.Warn().Err(err).Str("index", idx).Msg("Failed to create index")
        }
    }

    // # Create continuous aggregates for hourly and daily aggregations
    aggregates := []string{
        `CREATE MATERIALIZED VIEW IF NOT EXISTS cpu_metrics_hourly
            WITH (timescaledb.continuous) AS
            SELECT 
                time_bucket('1 hour', timestamp) AS bucket,
                hostname,
                instance_id,
                AVG(usage_percent) as avg_usage,
                MAX(usage_percent) as max_usage,
                MIN(usage_percent) as min_usage,
                percentile_cont(0.95) WITHIN GROUP (ORDER BY usage_percent) as p95_usage
            FROM cpu_metrics
            GROUP BY bucket, hostname, instance_id
            WITH NO DATA`,
            
        `CREATE MATERIALIZED VIEW IF NOT EXISTS cpu_metrics_daily
            WITH (timescaledb.continuous) AS
            SELECT 
                time_bucket('1 day', timestamp) AS bucket,
                hostname,
                instance_id,
                AVG(usage_percent) as avg_usage,
                MAX(usage_percent) as max_usage,
                MIN(usage_percent) as min_usage,
                percentile_cont(0.95) WITHIN GROUP (ORDER BY usage_percent) as p95_usage
            FROM cpu_metrics
            GROUP BY bucket, hostname, instance_id
            WITH NO DATA`,
    }

    for _, agg := range aggregates {
        _, err = c.pool.Exec(ctx, agg)
        if err != nil {
            log.Warn().Err(err).Msg("Failed to create continuous aggregate")
        }
    }

    // # Add refresh policies for continuous aggregates
    _, err = c.pool.Exec(ctx, `
        SELECT add_continuous_aggregate_policy('cpu_metrics_hourly',
            start_offset => INTERVAL '2 hours',
            end_offset => INTERVAL '1 hour',
            schedule_interval => INTERVAL '1 hour',
            if_not_exists => TRUE
        )
    `)
    if err != nil {
        log.Warn().Err(err).Msg("Failed to add hourly aggregate policy")
    }

    _, err = c.pool.Exec(ctx, `
        SELECT add_continuous_aggregate_policy('cpu_metrics_daily',
            start_offset => INTERVAL '2 days',
            end_offset => INTERVAL '1 day',
            schedule_interval => INTERVAL '1 day',
            if_not_exists => TRUE
        )
    `)
    if err != nil {
        log.Warn().Err(err).Msg("Failed to add daily aggregate policy")
    }

    log.Info().Msg("Database migrations completed successfully")
    return nil
}

// # Close gracefully shuts down the connection pool
func (c *Client) Close() {
    if c.pool != nil {
        c.pool.Close()
        log.Info().Msg("TimescaleDB connection pool closed")
    }
}
