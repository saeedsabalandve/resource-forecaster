package collector

import (
    "context"
    "fmt"
    "os/exec"
    "strconv"
    "strings"

    "github.com/rs/zerolog/log"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
)

// # GPUGatherer collects NVIDIA GPU metrics using nvidia-smi
// # Supports multi-GPU setups and GPU-accelerated ML workloads
type GPUGatherer struct {
    nvidiaSmiPath string
    gpuCount      int
    enabled       bool
}

// # NewGPUGatherer creates GPU metrics collector with capability detection
func NewGPUGatherer() *GPUGatherer {
    g := &GPUGatherer{
        nvidiaSmiPath: "nvidia-smi",
        enabled:       false,
    }

    // # Detect if NVIDIA drivers and GPU are available
    if g.detectGPU() {
        g.enabled = true
        log.Info().Int("gpu_count", g.gpuCount).Msg("GPU detected and enabled for metric collection")
    } else {
        log.Info().Msg("No GPU detected, GPU metric collection disabled")
    }

    return g
}

// # Name returns the gatherer identifier
func (g *GPUGatherer) Name() string {
    return "gpu"
}

// # Gather collects GPU metrics from all available GPUs
func (g *GPUGatherer) Gather(ctx context.Context) (*ResourceMetrics, error) {
    if !g.enabled {
        return &ResourceMetrics{}, nil
    }

    tracer := otel.Tracer("gpu-gatherer")
    ctx, span := tracer.Start(ctx, "gather-gpu-metrics")
    defer span.End()

    metrics := &ResourceMetrics{
        GPU: make([]GPUMetrics, 0, g.gpuCount),
    }

    // # Query GPU metrics using nvidia-smi with CSV output for parsing
    cmd := exec.CommandContext(ctx, g.nvidiaSmiPath,
        "--query-gpu=index,name,utilization.gpu,memory.total,memory.used,temperature.gpu,power.draw",
        "--format=csv,noheader,nounits",
    )

    output, err := cmd.Output()
    if err != nil {
        span.RecordError(err)
        return metrics, fmt.Errorf("failed to execute nvidia-smi: %w", err)
    }

    // # Parse CSV output for each GPU
    lines := strings.Split(strings.TrimSpace(string(output)), "\n")
    span.SetAttributes(attribute.Int("gpu_count", len(lines)))

    for _, line := range lines {
        fields := strings.Split(line, ",")
        if len(fields) < 7 {
            log.Warn().Str("line", line).Msg("Malformed nvidia-smi output")
            continue
        }

        gpuMetrics := GPUMetrics{}
        
        // # Parse each field with error handling
        if idx, err := strconv.Atoi(strings.TrimSpace(fields[0])); err == nil {
            gpuMetrics.Index = idx
        }
        
        gpuMetrics.Name = strings.TrimSpace(fields[1])
        
        if util, err := strconv.ParseFloat(strings.TrimSpace(fields[2]), 64); err == nil {
            gpuMetrics.UtilizationPercent = util
        }
        
        if memTotal, err := strconv.ParseUint(strings.TrimSpace(fields[3]), 10, 64); err == nil {
            gpuMetrics.MemoryTotalBytes = memTotal * 1024 * 1024 // # Convert MB to bytes
        }
        
        if memUsed, err := strconv.ParseUint(strings.TrimSpace(fields[4]), 10, 64); err == nil {
            gpuMetrics.MemoryUsedBytes = memUsed * 1024 * 1024
        }
        
        if temp, err := strconv.ParseFloat(strings.TrimSpace(fields[5]), 64); err == nil {
            gpuMetrics.TemperatureCelsius = temp
        }
        
        if power, err := strconv.ParseFloat(strings.TrimSpace(fields[6]), 64); err == nil {
            gpuMetrics.PowerUsageWatts = power
        }

        metrics.GPU = append(metrics.GPU, gpuMetrics)

        log.Debug().
            Int("gpu_index", gpuMetrics.Index).
            Str("name", gpuMetrics.Name).
            Float64("utilization", gpuMetrics.UtilizationPercent).
            Float64("temperature", gpuMetrics.TemperatureCelsius).
            Msg("GPU metrics collected")
    }

    return metrics, nil
}

// # detectGPU checks if NVIDIA GPUs are available
func (g *GPUGatherer) detectGPU() bool {
    cmd := exec.Command(g.nvidiaSmiPath, "-L")
    output, err := cmd.Output()
    if err != nil {
        return false
    }

    // # Count number of GPUs
    g.gpuCount = strings.Count(string(output), "GPU ")
    return g.gpuCount > 0
}

// # GetGPUUtilization returns aggregate GPU metrics for monitoring
func (g *GPUGatherer) GetGPUUtilization(ctx context.Context) (float64, error) {
    if !g.enabled {
        return 0, nil
    }

    cmd := exec.CommandContext(ctx, g.nvidiaSmiPath,
        "--query-gpu=utilization.gpu",
        "--format=csv,noheader,nounits",
    )

    output, err := cmd.Output()
    if err != nil {
        return 0, err
    }

    // # Calculate average GPU utilization
    lines := strings.Split(strings.TrimSpace(string(output)), "\n")
    if len(lines) == 0 {
        return 0, nil
    }

    var totalUtil float64
    for _, line := range lines {
        if util, err := strconv.ParseFloat(strings.TrimSpace(line), 64); err == nil {
            totalUtil += util
        }
    }

    return totalUtil / float64(len(lines)), nil
}
