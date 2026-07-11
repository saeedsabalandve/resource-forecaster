package handlers

import (
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "strconv"
    "time"

    "github.com/go-chi/chi/v5"
    "github.com/rs/zerolog/log"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/codes"
    "go.opentelemetry.io/otel/metric"

    "resource-forecaster/internal/forecaster"
    "resource-forecaster/internal/forecaster/models"
    "resource-forecaster/internal/storage/timescale"
)

// # ForecastHandler handles forecasting API endpoints
type ForecastHandler struct {
    forecaster *forecaster.Engine
    storage    *timescale.Client
    meter      metric.Meter
    
    // # Metrics for observability
    forecastRequests  metric.Int64Counter
    forecastDuration  metric.Float64Histogram
    forecastErrors    metric.Int64Counter
    dataPointsFetched metric.Int64Histogram
}

// # ForecastRequest represents API request for resource forecasting
type ForecastRequest struct {
    Hostname           string    `json:"hostname" validate:"required"`
    MetricType         string    `json:"metricType" validate:"required,oneof=cpu memory disk network"`
    PeriodHours        int       `json:"periodHours" validate:"required,min=1,max=720"`  // # Max 30 days
    ConfidenceInterval float64   `json:"confidenceInterval" validate:"omitempty,min=0.5,max=0.99"`
    IncludeBreakdown   bool      `json:"includeBreakdown"`
    StartTime          time.Time `json:"startTime,omitempty"`  // # Optional custom time range
    EndTime            time.Time `json:"endTime,omitempty"`
}

// # ForecastResponse contains the forecast results with metadata
type ForecastResponse struct {
    RequestID        string                     `json:"requestId"`
    Hostname         string                     `json:"hostname"`
    MetricType       string                     `json:"metricType"`
    GeneratedAt      time.Time                  `json:"generatedAt"`
    ForecastHorizon  int                        `json:"forecastHorizon"`  // # In hours
    DataPointsUsed   int                        `json:"dataPointsUsed"`
    Forecast         []ForecastDataPoint        `json:"forecast"`
    ConfidenceScore  float64                    `json:"confidenceScore"`
    ModelPerformance map[string]ModelPerformance `json:"modelPerformance,omitempty"`
    Recommendations  []string                   `json:"recommendations,omitempty"`
    Metadata         ForecastMetadata           `json:"metadata"`
}

type ForecastDataPoint struct {
    Timestamp      time.Time `json:"timestamp"`
    PredictedValue float64   `json:"predictedValue"`
    LowerBound     float64   `json:"lowerBound"`    // # Lower confidence interval
    UpperBound     float64   `json:"upperBound"`    // # Upper confidence interval
    AnomalyScore   float64   `json:"anomalyScore,omitempty"`  // # 0-1, higher means more anomalous
    TrendDirection string    `json:"trendDirection,omitempty"` // # "increasing", "decreasing", "stable"
}

type ModelPerformance struct {
    ModelName string  `json:"modelName"`
    MAPE      float64 `json:"mape"`     // # Mean Absolute Percentage Error
    RMSE      float64 `json:"rmse"`     // # Root Mean Square Error
    Weight    float64 `json:"weight"`   // # Weight in ensemble
}

type ForecastMetadata struct {
    TrainingDurationMs  int64   `json:"trainingDurationMs"`
    PredictionDurationMs int64  `json:"predictionDurationMs"`
    EnsembleStrategy    string  `json:"ensembleStrategy"`
    SeasonalPattern     string  `json:"seasonalPattern,omitempty"` // # e.g., "hourly", "daily", "weekly"
    TrendStrength       float64 `json:"trendStrength,omitempty"`   // # 0-1 strength of trend
    Volatility          float64 `json:"volatility"`                // # Coefficient of variation
}

// # NewForecastHandler creates handler with all dependencies
func NewForecastHandler(forecaster *forecaster.Engine, storage *timescale.Client) *ForecastHandler {
    meter := otel.Meter("forecast-handler")
    
    forecastRequests, _ := meter.Int64Counter(
        "forecast_requests_total",
        metric.WithDescription("Total number of forecast requests"),
    )
    
    forecastDuration, _ := meter.Float64Histogram(
        "forecast_generation_duration_seconds",
        metric.WithDescription("Duration of forecast generation"),
        metric.WithExplicitBucketBoundaries(0.1, 0.5, 1, 2, 5, 10, 30),
    )
    
    forecastErrors, _ := meter.Int64Counter(
        "forecast_errors_total",
        metric.WithDescription("Total number of forecast errors"),
    )
    
    dataPointsFetched, _ := meter.Int64Histogram(
        "forecast_data_points_fetched",
        metric.WithDescription("Number of historical data points used for forecasting"),
    )
    
    return &ForecastHandler{
        forecaster:        forecaster,
        storage:           storage,
        meter:             meter,
        forecastRequests:  forecastRequests,
        forecastDuration:  forecastDuration,
        forecastErrors:    forecastErrors,
        dataPointsFetched: dataPointsFetched,
    }
}

// # GenerateForecast handles POST /api/v1/forecast
func (h *ForecastHandler) GenerateForecast(w http.ResponseWriter, r *http.Request) {
    tracer := otel.Tracer("forecast-handler")
    ctx, span := tracer.Start(r.Context(), "generate-forecast")
    defer span.End()
    
    requestID := r.Context().Value(ContextKeyRequestID).(string)
    span.SetAttributes(attribute.String("request_id", requestID))
    
    // # Parse and validate request
    var req ForecastRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        span.RecordError(err)
        span.SetStatus(codes.Error, "invalid request body")
        writeError(w, http.StatusBadRequest, "invalid_request", "Failed to parse request body")
        return
    }
    
    // # Validate request parameters
    if err := validateForecastRequest(&req); err != nil {
        span.SetStatus(codes.Error, err.Error())
        writeError(w, http.StatusBadRequest, "validation_error", err.Error())
        return
    }
    
    // # Set default confidence interval if not specified
    if req.ConfidenceInterval == 0 {
        req.ConfidenceInterval = 0.95
    }
    
    span.SetAttributes(
        attribute.String("hostname", req.Hostname),
        attribute.String("metric_type", req.MetricType),
        attribute.Int("period_hours", req.PeriodHours),
        attribute.Float64("confidence_interval", req.ConfidenceInterval),
    )
    
    startTime := time.Now()
    
    // # Fetch historical data from TimescaleDB
    histStart, histEnd := h.calculateHistoricalRange(req)
    
    timestamps, values, err := h.storage.GetHistoricalMetrics(
        ctx, req.Hostname, req.MetricType, histStart, histEnd,
    )
    if err != nil {
        h.forecastErrors.Add(ctx, 1, metric.WithAttributes(
            attribute.String("error_type", "data_fetch"),
        ))
        span.RecordError(err)
        span.SetStatus(codes.Error, "failed to fetch historical data")
        log.Error().Err(err).Str("request_id", requestID).Msg("Failed to fetch historical metrics")
        writeError(w, http.StatusInternalServerError, "data_fetch_error", 
            "Failed to fetch historical metrics")
        return
    }
    
    h.dataPointsFetched.Record(ctx, int64(len(timestamps)))
    
    // # Check minimum data requirements
    minDataPoints := h.forecaster.GetMinDataPoints()
    if len(timestamps) < minDataPoints {
        h.forecastErrors.Add(ctx, 1, metric.WithAttributes(
            attribute.String("error_type", "insufficient_data"),
        ))
        span.SetStatus(codes.Error, "insufficient data points")
        writeError(w, http.StatusBadRequest, "insufficient_data",
            fmt.Sprintf("Need at least %d data points, got %d. Please collect more metrics first.", 
                minDataPoints, len(timestamps)))
        return
    }
    
    // # Train forecasting models
    trainingStart := time.Now()
    if err := h.forecaster.TrainModels(ctx, req.Hostname, req.MetricType, timestamps, values); err != nil {
        h.forecastErrors.Add(ctx, 1, metric.WithAttributes(
            attribute.String("error_type", "training"),
        ))
        span.RecordError(err)
        span.SetStatus(codes.Error, "model training failed")
        log.Error().Err(err).Str("request_id", requestID).Msg("Model training failed")
        writeError(w, http.StatusInternalServerError, "training_error", 
            "Failed to train forecasting models")
        return
    }
    trainingDuration := time.Since(trainingStart)
    
    // # Generate forecast
    predictionStart := time.Now()
    forecast, err := h.forecaster.GenerateForecast(ctx, req.Hostname, req.MetricType, req.PeriodHours)
    if err != nil {
        h.forecastErrors.Add(ctx, 1, metric.WithAttributes(
            attribute.String("error_type", "prediction"),
        ))
        span.RecordError(err)
        span.SetStatus(codes.Error, "forecast generation failed")
        log.Error().Err(err).Str("request_id", requestID).Msg("Forecast generation failed")
        writeError(w, http.StatusInternalServerError, "forecast_error", 
            "Failed to generate forecast")
        return
    }
    predictionDuration := time.Since(predictionStart)
    
    // # Build response
    response := h.buildForecastResponse(
        requestID, req, forecast, 
        len(timestamps), 
        trainingDuration, predictionDuration,
    )
    
    // # Add anomaly detection results
    response = h.enrichWithAnomalyDetection(response, timestamps, values)
    
    // # Generate recommendations based on forecast
    response.Recommendations = h.generateRecommendations(req.MetricType, forecast)
    
    // # Record metrics
    totalDuration := time.Since(startTime)
    h.forecastDuration.Record(ctx, totalDuration.Seconds(), metric.WithAttributes(
        attribute.String("metric_type", req.MetricType),
        attribute.Int("data_points", len(timestamps)),
    ))
    h.forecastRequests.Add(ctx, 1, metric.WithAttributes(
        attribute.String("status", "success"),
    ))
    
    span.SetStatus(codes.Ok, "forecast generated successfully")
    span.SetAttributes(
        attribute.Int("data_points_used", len(timestamps)),
        attribute.Float64("forecast_confidence", forecast.Accuracy),
        attribute.Int64("training_ms", trainingDuration.Milliseconds()),
        attribute.Int64("prediction_ms", predictionDuration.Milliseconds()),
    )
    
    // # Write response
    writeJSON(w, http.StatusOK, response)
    
    log.Info().
        Str("request_id", requestID).
        Str("hostname", req.Hostname).
        Str("metric_type", req.MetricType).
        Int("forecast_hours", req.PeriodHours).
        Int("data_points", len(timestamps)).
        Float64("confidence", forecast.Accuracy).
        Dur("total_duration", totalDuration).
        Msg("Forecast generated successfully")
}

// # GetForecastHistory handles GET /api/v1/forecast/history
func (h *ForecastHandler) GetForecastHistory(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    requestID := r.Context().Value(ContextKeyRequestID).(string)
    
    hostname := chi.URLParam(r, "hostname")
    metricType := r.URL.Query().Get("metricType")
    limitStr := r.URL.Query().Get("limit")
    
    limit := 24 // # Default: last 24 forecasts
    if limitStr != "" {
        if parsedLimit, err := strconv.Atoi(limitStr); err == nil && parsedLimit > 0 {
            limit = parsedLimit
        }
    }
    
    // # Fetch forecast history from database
    forecasts, err := h.storage.GetForecastHistory(ctx, hostname, metricType, limit)
    if err != nil {
        log.Error().Err(err).Str("request_id", requestID).Msg("Failed to fetch forecast history")
        writeError(w, http.StatusInternalServerError, "fetch_error", "Failed to fetch forecast history")
        return
    }
    
    writeJSON(w, http.StatusOK, map[string]interface{}{
        "requestId": requestID,
        "hostname":  hostname,
        "forecasts": forecasts,
        "count":     len(forecasts),
    })
}

// # CompareForecastWithActual handles GET /api/v1/forecast/accuracy/{hostname}
func (h *ForecastHandler) CompareForecastWithActual(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    requestID := r.Context().Value(ContextKeyRequestID).(string)
    
    hostname := chi.URLParam(r, "hostname")
    metricType := r.URL.Query().Get("metricType")
    
    // # Fetch historical forecasts and actual values
    comparison, err := h.forecaster.EvaluateForecastAccuracy(ctx, hostname, metricType)
    if err != nil {
        log.Error().Err(err).Str("request_id", requestID).Msg("Failed to compare forecasts")
        writeError(w, http.StatusInternalServerError, "comparison_error", 
            "Failed to compare forecasts with actual values")
        return
    }
    
    writeJSON(w, http.StatusOK, map[string]interface{}{
        "requestId":  requestID,
        "hostname":   hostname,
        "metricType": metricType,
        "accuracy":   comparison,
    })
}

// # calculateHistoricalRange determines optimal historical data range for training
func (h *ForecastHandler) calculateHistoricalRange(req ForecastRequest) (time.Time, time.Time) {
    endTime := time.Now()
    if !req.EndTime.IsZero() {
        endTime = req.EndTime
    }
    
    // # Use at least 2x the forecast horizon for training, minimum 24 hours
    trainingHours := req.PeriodHours * 2
    if trainingHours < 24 {
        trainingHours = 24
    }
    
    startTime := endTime.Add(-time.Duration(trainingHours) * time.Hour)
    if !req.StartTime.IsZero() {
        startTime = req.StartTime
    }
    
    return startTime, endTime
}

// # buildForecastResponse constructs the API response from model output
func (h *ForecastHandler) buildForecastResponse(
    requestID string,
    req ForecastRequest,
    forecast *models.ForecastResult,
    dataPointsUsed int,
    trainingDuration, predictionDuration time.Duration,
) ForecastResponse {
    response := ForecastResponse{
        RequestID:       requestID,
        Hostname:        req.Hostname,
        MetricType:      req.MetricType,
        GeneratedAt:     time.Now(),
        ForecastHorizon: req.PeriodHours,
        DataPointsUsed:  dataPointsUsed,
        ConfidenceScore: forecast.Accuracy,
        Metadata: ForecastMetadata{
            TrainingDurationMs:   trainingDuration.Milliseconds(),
            PredictionDurationMs: predictionDuration.Milliseconds(),
            EnsembleStrategy:     "weighted_average",
        },
    }
    
    // # Build forecast data points
    for i := range forecast.Timestamps {
        if i >= req.PeriodHours {
            break
        }
        
        dataPoint := ForecastDataPoint{
            Timestamp:      forecast.Timestamps[i],
            PredictedValue: forecast.PredictedValues[i],
        }
        
        // # Add confidence intervals if available
        if i < len(forecast.LowerBound) {
            dataPoint.LowerBound = forecast.LowerBound[i]
            dataPoint.UpperBound = forecast.UpperBound[i]
        }
        
        // # Calculate trend direction
        if i > 0 {
            if forecast.PredictedValues[i] > forecast.PredictedValues[i-1]*1.05 {
                dataPoint.TrendDirection = "increasing"
            } else if forecast.PredictedValues[i] < forecast.PredictedValues[i-1]*0.95 {
                dataPoint.TrendDirection = "decreasing"
            } else {
                dataPoint.TrendDirection = "stable"
            }
        }
        
        response.Forecast = append(response.Forecast, dataPoint)
    }
    
    return response
}

// # enrichWithAnomalyDetection adds anomaly scores to forecast data points
func (h *ForecastHandler) enrichWithAnomalyDetection(
    response ForecastResponse,
    historicalTimestamps []time.Time,
    historicalValues []float64,
) ForecastResponse {
    // # Calculate statistics from historical data
    mean, stddev := calculateStatistics(historicalValues)
    
    for i := range response.Forecast {
        // # Calculate z-score for anomaly detection
        zscore := math.Abs((response.Forecast[i].PredictedValue - mean) / stddev)
        
        // # Map z-score to anomaly score (sigmoid-like function)
        response.Forecast[i].AnomalyScore = 1.0 / (1.0 + math.Exp(-(zscore-2.0)))
        
        if response.Forecast[i].AnomalyScore > 0.7 {
            response.Forecast[i].TrendDirection = "anomalous_" + response.Forecast[i].TrendDirection
        }
    }
    
    return response
}

// # generateRecommendations provides actionable insights based on forecast
func (h *ForecastHandler) generateRecommendations(metricType string, forecast *models.ForecastResult) []string {
    var recommendations []string
    
    // # Calculate forecast statistics
    avgPredicted := calculateAverage(forecast.PredictedValues)
    maxPredicted := calculateMax(forecast.PredictedValues)
    
    switch metricType {
    case "cpu":
        if avgPredicted > 80 {
            recommendations = append(recommendations, 
                "CRITICAL: Average CPU usage forecasted above 80%. Consider scaling up or optimizing workloads.")
        }
        if maxPredicted > 95 {
            recommendations = append(recommendations,
                "WARNING: Peak CPU usage may exceed 95%. Immediate scaling recommended to prevent throttling.")
        }
        if avgPredicted < 30 {
            recommendations = append(recommendations,
                "INFO: Low CPU utilization forecasted. Consider downsizing to reduce costs.")
        }
        
    case "memory":
        if avgPredicted > 85 {
            recommendations = append(recommendations,
                "CRITICAL: Memory usage forecasted above 85%. Risk of OOM kills. Increase memory allocation.")
        }
        if isTrendIncreasing(forecast.PredictedValues) {
            recommendations = append(recommendations,
                "WARNING: Memory usage shows increasing trend. Possible memory leak. Investigate applications.")
        }
        
    case "disk":
        if avgPredicted > 90 {
            recommendations = append(recommendations,
                "CRITICAL: Disk usage forecasted above 90%. Add storage or implement cleanup policies.")
        }
        
    case "network":
        if maxPredicted > 80 {
            recommendations = append(recommendations,
                "INFO: Network bandwidth may be constrained during peak. Consider upgrading network tier.")
        }
    }
    
    // # Add confidence-based recommendation
    if forecast.Accuracy < 0.7 {
        recommendations = append(recommendations,
            "NOTE: Low forecast confidence. Consider collecting more historical data for better accuracy.")
    }
    
    return recommendations
}

// # validateForecastRequest validates all request parameters
func validateForecastRequest(req *ForecastRequest) error {
    if req.Hostname == "" {
        return fmt.Errorf("hostname is required")
    }
    
    validMetrics := map[string]bool{"cpu": true, "memory": true, "disk": true, "network": true}
    if !validMetrics[req.MetricType] {
        return fmt.Errorf("invalid metric type: %s", req.MetricType)
    }
    
    if req.PeriodHours < 1 || req.PeriodHours > 720 {
        return fmt.Errorf("period hours must be between 1 and 720")
    }
    
    if req.ConfidenceInterval != 0 && (req.ConfidenceInterval < 0.5 || req.ConfidenceInterval > 0.99) {
        return fmt.Errorf("confidence interval must be between 0.5 and 0.99")
    }
    
    return nil
}

// # Helper functions for statistical calculations
func calculateStatistics(values []float64) (mean, stddev float64) {
    if len(values) == 0 {
        return 0, 0
    }
    
    sum := 0.0
    for _, v := range values {
        sum += v
    }
    mean = sum / float64(len(values))
    
    variance := 0.0
    for _, v := range values {
        diff := v - mean
        variance += diff * diff
    }
    variance /= float64(len(values))
    stddev = math.Sqrt(variance)
    
    return
}

func calculateAverage(values []float64) float64 {
    if len(values) == 0 {
        return 0
    }
    sum := 0.0
    for _, v := range values {
        sum += v
    }
    return sum / float64(len(values))
}

func calculateMax(values []float64) float64 {
    if len(values) == 0 {
        return 0
    }
    max := values[0]
    for _, v := range values {
        if v > max {
            max = v
        }
    }
    return max
}

func isTrendIncreasing(values []float64) bool {
    if len(values) < 2 {
        return false
    }
    // # Linear regression slope
    n := float64(len(values))
    var sumX, sumY, sumXY, sumX2 float64
    for i, y := range values {
        x := float64(i)
        sumX += x
        sumY += y
        sumXY += x * y
        sumX2 += x * x
    }
    slope := (n*sumXY - sumX*sumY) / (n*sumX2 - sumX*sumX)
    return slope > 0
}
