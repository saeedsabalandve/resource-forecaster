package collector

import (
    "context"
    "sync"
    "time"

    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
    "github.com/rs/zerolog/log"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"

    "resource-forecaster/internal/storage/timescale"
)

// # MetricGatherer interface for pluggable metric collection
// # Allows adding new resource types without modifying collector core
type MetricGatherer interface {
    Name() string
    Gather(ctx context.Context) (*ResourceMetrics, error)
}

// # Core resource metrics structure with comprehensive resource tracking
type ResourceMetrics struct {
    Timestamp      time.Time
    Hostname       string
    InstanceID     string              // # Cloud instance ID (i-xxxx for AWS, VM ID for Azure)
    InstanceType   string              // # e.g., m5.xlarge, Standard_D4s_v3
    CPU            CPUMetrics
    Memory         MemoryMetrics
    Disk           []DiskMetrics
    Network        NetworkMetrics
    GPU            []GPUMetrics        // # For GPU-enabled instances
    ProcessCount   int                 // # Number of running processes
    LoadAverage    LoadAverageMetrics
    ContainerStats []ContainerMetrics  // # If running in containerized environment
}

type CPUMetrics struct {
    UsagePercent      float64
    UserPercent       float64
    SystemPercent     float64
    IOWaitPercent     float64
    StealPercent      float64 // # Important for virtualized environments
    NumCores          int
    ThrottledTime     float64 // # CPU throttling in seconds (container environments)
}

type MemoryMetrics struct {
    TotalBytes        uint64
    UsedBytes         uint64
    AvailableBytes    uint64
    UsedPercent       float64
    SwapTotalBytes    uint64
    SwapUsedBytes     uint64
    CachedBytes       uint64
    BuffersBytes      uint64
}

type DiskMetrics struct {
    MountPoint        string
    Device            string
    TotalBytes        uint64
    UsedBytes         uint64
    AvailableBytes    uint64
    UsedPercent       float64
    ReadBytesPerSec   float64
    WriteBytesPerSec  float64
    IOPS              float64
}

type NetworkMetrics struct {
    InterfaceName     string
    ReceivedBytesPerSec float64
    TransmittedBytesPerSec float64
    PacketsDropped    uint64
    ErrorCount        uint64
    TCPConnections    int
}

type GPUMetrics struct {
    Index             int
    Name              string
    UtilizationPercent float64
    MemoryTotalBytes  uint64
    MemoryUsedBytes   uint64
    TemperatureCelsius float64
    PowerUsageWatts   float64
}

type LoadAverageMetrics struct {
    Load1             float64
    Load5             float64
    Load15            float64
}

type ContainerMetrics struct {
    ContainerID       string
    Name              string
    CPULimitCores     float64
    CPUUsagePercent   float64
    MemoryLimitBytes  uint64
    MemoryUsageBytes  uint64
}

// # Collector orchestrates metric gathering with proper concurrency control
type Collector struct {
    storage    *timescale.Client
    gatherers  []MetricGatherer
    hostname   string
    instanceID string
    
    // # Prometheus metrics for observability of the collector itself
    collectionDuration prometheus.Histogram
    collectionErrors   prometheus.Counter
    metricsCollected   prometheus.Counter
}

// # CollectorOption functional options pattern for flexible configuration
type CollectorOption func(*Collector)

func WithGatherers(gatherers ...MetricGatherer) CollectorOption {
    return func(c *Collector) {
        c.gatherers = append(c.gatherers, gatherers...)
    }
}

// # NewCollector creates a production-ready metric collector
func NewCollector(storage *timescale.Client, opts ...CollectorOption) *Collector {
    c := &Collector{
        storage: storage,
        collectionDuration: promauto.NewHistogram(prometheus.HistogramOpts{
            Name: "resource_collection_duration_seconds",
            Help: "Duration of resource metric collection",
            Buckets: []float64{.1, .25, .5, 1, 2.5, 5, 10},
        }),
        collectionErrors: promauto.NewCounter(prometheus.CounterOpts{
            Name: "resource_collection_errors_total",
            Help: "Total number of metric collection errors",
        }),
        metricsCollected: promauto.NewCounter(prometheus.CounterOpts{
            Name: "resource_metrics_collected_total",
            Help: "Total number of metric data points collected",
        }),
    }

    for _, opt := range opts {
        opt(c)
    }

    // # Auto-detect hostname and cloud instance metadata
    c.hostname = detectHostname()
    c.instanceID = detectCloudInstanceID()

    return c
}

// # Collect gathers all resource metrics concurrently and stores them
func (c *Collector) Collect(ctx context.Context) error {
    // # Create trace span for collection operation
    tracer := otel.Tracer("resource-collector")
    ctx, span := tracer.Start(ctx, "collect-metrics")
    defer span.End()

    startTime := time.Now()
    
    // # Gather metrics from all sources concurrently
    var wg sync.WaitGroup
    resultsCh := make(chan *ResourceMetrics, len(c.gatherers))
    errorsCh := make(chan error, len(c.gatherers))

    for _, gatherer := range c.gatherers {
        wg.Add(1)
        go func(g MetricGatherer) {
            defer wg.Done()
            
            metrics, err := g.Gather(ctx)
            if err != nil {
                c.collectionErrors.Inc()
                log.Error().
                    Err(err).
                    Str("gatherer", g.Name()).
                    Msg("Failed to gather metrics")
                errorsCh <- fmt.Errorf("gatherer %s: %w", g.Name(), err)
                return
            }
            
            resultsCh <- metrics
        }(gatherer)
    }

    // # Wait for all goroutines to complete
    go func() {
        wg.Wait()
        close(resultsCh)
        close(errorsCh)
    }()

    // # Aggregate all collected metrics
    aggregated := &ResourceMetrics{
        Timestamp: time.Now(),
        Hostname:  c.hostname,
        InstanceID: c.instanceID,
    }

    for metrics := range resultsCh {
        c.metricsCollected.Inc()
        c.mergeMetrics(aggregated, metrics)
    }

    // # Log any non-critical errors
    for err := range errorsCh {
        log.Warn().Err(err).Msg("Non-critical metric collection error")
    }

    // # Store metrics in TimescaleDB with retry logic
    if err := c.storage.StoreMetrics(ctx, aggregated); err != nil {
        span.RecordError(err)
        span.SetAttributes(attribute.String("error.type", "storage_error"))
        return fmt.Errorf("failed to store metrics: %w", err)
    }

    // # Record collection duration
    duration := time.Since(startTime)
    c.collectionDuration.Observe(duration.Seconds())

    log.Debug().
        Dur("duration", duration).
        Str("hostname", c.hostname).
        Msg("Metrics collected successfully")

    return nil
}

// # Merge metrics from different gatherers into single structure
func (c *Collector) mergeMetrics(target *ResourceMetrics, source *ResourceMetrics) {
    if source.CPU.NumCores > 0 {
        target.CPU = source.CPU
    }
    if source.Memory.TotalBytes > 0 {
        target.Memory = source.Memory
    }
    if len(source.Disk) > 0 {
        target.Disk = append(target.Disk, source.Disk...)
    }
    if len(source.Network.InterfaceName) > 0 {
        target.Network = source.Network
    }
    if len(source.GPU) > 0 {
        target.GPU = append(target.GPU, source.GPU...)
    }
    if source.LoadAverage.Load1 > 0 {
        target.LoadAverage = source.LoadAverage
    }
    if len(source.ContainerStats) > 0 {
        target.ContainerStats = append(target.ContainerStats, source.ContainerStats...)
    }
}

// # Detect cloud instance ID for cost attribution and auto-scaling integration
func detectCloudInstanceID() string {
    // # AWS IMDSv2 metadata endpoint (token-protected)
    if data, err := ioutil.ReadFile("/sys/class/dmi/id/product_uuid"); err == nil {
        return strings.TrimSpace(string(data))
    }
    // # Azure Instance Metadata Service
    if resp, err := http.Get("http://169.254.169.254/metadata/instance/compute/vmId?api-version=2021-02-01&format=text"); err == nil {
        if body, err := ioutil.ReadAll(resp.Body); err == nil {
            return strings.TrimSpace(string(body))
        }
    }
    return "unknown"
}
