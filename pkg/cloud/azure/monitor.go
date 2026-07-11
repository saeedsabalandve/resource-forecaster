package azure

import (
    "context"
    "fmt"
    "time"

    "github.com/Azure/azure-sdk-for-go/sdk/azcore"
    "github.com/Azure/azure-sdk-for-go/sdk/azidentity"
    "github.com/Azure/azure-sdk-for-go/sdk/monitor/azquery"
    "github.com/rs/zerolog/log"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
)

// # AzureMonitorClient wraps Azure Monitor Query SDK for metric collection
type AzureMonitorClient struct {
    metricsClient  *azquery.MetricsClient
    logsClient     *azquery.LogsClient
    subscriptionID string
    resourceGroup  string
}

// # AzureMetric represents a metric data point from Azure Monitor
type AzureMetric struct {
    Name          string
    Unit          string
    TimeSeries    []AzureTimeSeries
    Aggregation   string
}

// # AzureTimeSeries contains time-series data points
type AzureTimeSeries struct {
    Timestamps []time.Time
    Values     []float64
    Metadata   map[string]string
}

// # NewAzureMonitorClient creates authenticated Azure Monitor client
func NewAzureMonitorClient(subscriptionID, resourceGroup string) (*AzureMonitorClient, error) {
    // # Use Azure AD workload identity for authentication
    cred, err := azidentity.NewDefaultAzureCredential(nil)
    if err != nil {
        return nil, fmt.Errorf("failed to create Azure credential: %w", err)
    }

    metricsClient, err := azquery.NewMetricsClient(cred, nil)
    if err != nil {
        return nil, fmt.Errorf("failed to create metrics client: %w", err)
    }

    logsClient, err := azquery.NewLogsClient(cred, nil)
    if err != nil {
        return nil, fmt.Errorf("failed to create logs client: %w", err)
    }

    return &AzureMonitorClient{
        metricsClient:  metricsClient,
        logsClient:     logsClient,
        subscriptionID: subscriptionID,
        resourceGroup:  resourceGroup,
    }, nil
}

// # GetVMCPUUtilization retrieves CPU metrics for a specific VM
func (c *AzureMonitorClient) GetVMCPUUtilization(
    ctx context.Context,
    vmName string,
    startTime, endTime time.Time,
    interval time.Duration,
) (*AzureMetric, error) {
    tracer := otel.Tracer("azure-monitor")
    ctx, span := tracer.Start(ctx, "get-vm-cpu")
    defer span.End()

    span.SetAttributes(
        attribute.String("vm_name", vmName),
        attribute.String("resource_group", c.resourceGroup),
    )

    // # Build Azure resource URI
    resourceURI := fmt.Sprintf(
        "/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/virtualMachines/%s",
        c.subscriptionID, c.resourceGroup, vmName,
    )

    // # Build query parameters for CPU percentage
    timespan := fmt.Sprintf("%s/%s", 
        startTime.Format(time.RFC3339),
        endTime.Format(time.RFC3339),
    )

    resp, err := c.metricsClient.QueryResource(ctx, resourceURI,
        &azquery.MetricsClientQueryResourceOptions{
            Timespan:     &timespan,
            Interval:     toPtr(fmt.Sprintf("PT%dM", int(interval.Minutes()))),
            Metricnames:  toPtr("Percentage CPU"),
            Aggregation:  toPtr("Average,Maximum,Minimum"),
            Top:          toPtr[int32](1000),
            Orderby:      toPtr("Timestamp desc"),
            ResultType:   nil,
            Metricnamespace: toPtr("Microsoft.Compute/virtualMachines"),
        },
    )
    if err != nil {
        span.RecordError(err)
        return nil, fmt.Errorf("failed to query VM metrics: %w", err)
    }

    // # Parse response into common metric format
    metric := &AzureMetric{
        Name:        "Percentage CPU",
        Unit:        "Percent",
        Aggregation: "Average",
    }

    if len(resp.Value) > 0 {
        for _, ts := range resp.Value[0].Timeseries {
            azTS := AzureTimeSeries{
                Metadata: make(map[string]string),
            }
            
            for _, data := range ts.Data {
                if data.TimeStamp != nil && data.Average != nil {
                    azTS.Timestamps = append(azTS.Timestamps, *data.TimeStamp)
                    azTS.Values = append(azTS.Values, *data.Average)
                }
            }
            
            metric.TimeSeries = append(metric.TimeSeries, azTS)
        }
    }

    log.Debug().
        Str("vm_name", vmName).
        Int("data_points", len(metric.TimeSeries)).
        Msg("Retrieved VM CPU metrics from Azure Monitor")

    return metric, nil
}

// # GetVMMemoryUtilization retrieves memory metrics for a specific VM
func (c *AzureMonitorClient) GetVMMemoryUtilization(
    ctx context.Context,
    vmName string,
    startTime, endTime time.Time,
) (*AzureMetric, error) {
    resourceURI := fmt.Sprintf(
        "/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/virtualMachines/%s",
        c.subscriptionID, c.resourceGroup, vmName,
    )

    timespan := fmt.Sprintf("%s/%s", 
        startTime.Format(time.RFC3339),
        endTime.Format(time.RFC3339),
    )

    resp, err := c.metricsClient.QueryResource(ctx, resourceURI,
        &azquery.MetricsClientQueryResourceOptions{
            Timespan:     &timespan,
            Interval:     toPtr("PT5M"),
            Metricnames:  toPtr("Available Memory Bytes"),
            Aggregation:  toPtr("Average"),
            Metricnamespace: toPtr("Microsoft.Compute/virtualMachines"),
        },
    )
    if err != nil {
        return nil, fmt.Errorf("failed to query VM memory metrics: %w", err)
    }

    metric := &AzureMetric{
        Name:        "Available Memory Bytes",
        Unit:        "Bytes",
        Aggregation: "Average",
    }

    // # Parse response
    if len(resp.Value) > 0 {
        for _, ts := range resp.Value[0].Timeseries {
            azTS := AzureTimeSeries{}
            for _, data := range ts.Data {
                if data.TimeStamp != nil && data.Average != nil {
                    azTS.Timestamps = append(azTS.Timestamps, *data.TimeStamp)
                    azTS.Values = append(azTS.Values, *data.Average)
                }
            }
            metric.TimeSeries = append(metric.TimeSeries, azTS)
        }
    }

    return metric, nil
}

// # GetVMSSMetrics retrieves metrics for Virtual Machine Scale Sets
func (c *AzureMonitorClient) GetVMSSMetrics(
    ctx context.Context,
    vmssName string,
    metricNames []string,
    startTime, endTime time.Time,
) (map[string]*AzureMetric, error) {
    resourceURI := fmt.Sprintf(
        "/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/virtualMachineScaleSets/%s",
        c.subscriptionID, c.resourceGroup, vmssName,
    )

    timespan := fmt.Sprintf("%s/%s", 
        startTime.Format(time.RFC3339),
        endTime.Format(time.RFC3339),
    )

    metricsStr := ""
    for i, name := range metricNames {
        if i > 0 {
            metricsStr += ","
        }
        metricsStr += name
    }

    resp, err := c.metricsClient.QueryResource(ctx, resourceURI,
        &azquery.MetricsClientQueryResourceOptions{
            Timespan:    &timespan,
            Interval:    toPtr("PT5M"),
            Metricnames: &metricsStr,
            Aggregation: toPtr("Average,Maximum"),
            Metricnamespace: toPtr("Microsoft.Compute/virtualMachineScaleSets"),
        },
    )
    if err != nil {
        return nil, fmt.Errorf("failed to query VMSS metrics: %w", err)
    }

    result := make(map[string]*AzureMetric)
    
    for _, metricValue := range resp.Value {
        if metricValue.Name == nil {
            continue
        }

        metric := &AzureMetric{
            Name: *metricValue.Name,
            Unit: string(*metricValue.Unit),
        }

        for _, ts := range metricValue.Timeseries {
            azTS := AzureTimeSeries{
                Metadata: make(map[string]string),
            }
            
            for _, data := range ts.Data {
                if data.TimeStamp != nil && data.Average != nil {
                    azTS.Timestamps = append(azTS.Timestamps, *data.TimeStamp)
                    azTS.Values = append(azTS.Values, *data.Average)
                }
            }
            
            metric.TimeSeries = append(metric.TimeSeries, azTS)
        }

        result[*metricValue.Name] = metric
    }

    return result, nil
}

// # QueryLogs executes Kusto queries against Azure Log Analytics
func (c *AzureMonitorClient) QueryLogs(
    ctx context.Context,
    workspaceID string,
    query string,
    timespan time.Duration,
) ([]map[string]interface{}, error) {
    resp, err := c.logsClient.QueryWorkspace(ctx, workspaceID,
        azquery.Body{
            Query:    &query,
            Timespan: toPtr(fmt.Sprintf("PT%dH", int(timespan.Hours()))),
        },
        nil,
    )
    if err != nil {
        return nil, fmt.Errorf("failed to query logs: %w", err)
    }

    // # Parse query results
    var results []map[string]interface{}
    
    if resp.Tables != nil && len(resp.Tables) > 0 {
        table := resp.Tables[0]
        
        for _, row := range table.Rows {
            entry := make(map[string]interface{})
            for i, col := range table.Columns {
                if i < len(row) {
                    entry[*col.Name] = row[i]
                }
            }
            results = append(results, entry)
        }
    }

    return results, nil
}

// # Helper function for optional string pointer
func toPtr[T any](v T) *T {
    return &v
}
