package models

import (
    "context"
    "fmt"
    "math"
    "sort"
    "sync"
    "time"

    "github.com/rs/zerolog/log"
    "gonum.org/v1/gonum/stat"
)

// # ForecastResult holds the prediction with confidence intervals
type ForecastResult struct {
    Timestamps          []time.Time
    PredictedValues     []float64
    LowerBound          []float64  // # Lower confidence interval
    UpperBound          []float64  // # Upper confidence interval
    ModelName           string
    Accuracy            float64
    ConfidenceInterval  float64
    GeneratedAt         time.Time
}

// # ModelMetrics tracks individual model performance for weight adjustment
type ModelMetrics struct {
    ModelName           string
    MAPE                float64    // # Mean Absolute Percentage Error
    RMSE                float64    // # Root Mean Square Error
    MAE                 float64    // # Mean Absolute Error
    RecentAccuracy      float64
    DataPointsEvaluated int
    LastEvaluated       time.Time
}

// # Ensemble combines multiple forecasting models with dynamic weight adjustment
type Ensemble struct {
    mu              sync.RWMutex
    models          []ForecastingModel
    weights         map[string]float64
    strategy        EnsembleStrategy
    minDataPoints   int
    historyWindow   int             // # Number of historical predictions to consider for weight adjustment
    predictionCache map[string][]ForecastResult
}

// # ForecastingModel interface that all models must implement
type ForecastingModel interface {
    Name() string
    Train(ctx context.Context, timestamps []time.Time, values []float64) error
    Predict(ctx context.Context, horizon int) (*ForecastResult, error)
    Evaluate(actual, predicted []float64) *ModelMetrics
    SaveModel(path string) error
    LoadModel(path string) error
}

// # EnsembleStrategy defines how to combine model predictions
type EnsembleStrategy string

const (
    StrategyWeightedAverage  EnsembleStrategy = "weighted_average"
    StrategyStacking        EnsembleStrategy = "stacking"
    StrategyBestModel       EnsembleStrategy = "best_model"
    StrategyDynamicWeights  EnsembleStrategy = "dynamic_weights"
)

// # NewEnsemble creates ensemble with production-ready configuration
func NewEnsemble(models []ForecastingModel, strategy EnsembleStrategy, minDataPoints int) *Ensemble {
    e := &Ensemble{
        models:          models,
        weights:         make(map[string]float64),
        strategy:        strategy,
        minDataPoints:   minDataPoints,
        historyWindow:   10,
        predictionCache: make(map[string][]ForecastResult),
    }

    // # Initialize equal weights
    equalWeight := 1.0 / float64(len(models))
    for _, model := range models {
        e.weights[model.Name()] = equalWeight
    }

    return e
}

// # TrainAll trains all models in parallel for efficiency
func (e *Ensemble) TrainAll(ctx context.Context, timestamps []time.Time, values []float64) error {
    if len(timestamps) < e.minDataPoints {
        return fmt.Errorf("insufficient data points: %d (minimum: %d)", 
            len(timestamps), e.minDataPoints)
    }

    // # Validate input data quality
    if err := e.validateInputData(timestamps, values); err != nil {
        return fmt.Errorf("data validation failed: %w", err)
    }

    var wg sync.WaitGroup
    errCh := make(chan error, len(e.models))

    for _, model := range e.models {
        wg.Add(1)
        go func(m ForecastingModel) {
            defer wg.Done()
            
            log.Info().
                Str("model", m.Name()).
                Int("data_points", len(timestamps)).
                Msg("Training model")

            if err := m.Train(ctx, timestamps, values); err != nil {
                errCh <- fmt.Errorf("model %s training failed: %w", m.Name(), err)
                return
            }

            log.Info().
                Str("model", m.Name()).
                Msg("Model training completed")
        }(model)
    }

    wg.Wait()
    close(errCh)

    // # Collect training errors
    var errors []error
    for err := range errCh {
        errors = append(errors, err)
    }

    if len(errors) > 0 {
        // # If all models failed, return error; otherwise log warnings
        if len(errors) == len(e.models) {
            return fmt.Errorf("all models failed to train: %v", errors)
        }
        for _, err := range errors {
            log.Warn().Err(err).Msg("Some models failed to train")
        }
    }

    return nil
}

// # Predict combines predictions from all models using configured strategy
func (e *Ensemble) Predict(ctx context.Context, horizon int) (*ForecastResult, error) {
    e.mu.RLock()
    defer e.mu.RUnlock()

    var predictions []*ForecastResult
    
    // # Get predictions from all trained models concurrently
    var wg sync.WaitGroup
    predCh := make(chan *ForecastResult, len(e.models))

    for _, model := range e.models {
        wg.Add(1)
        go func(m ForecastingModel) {
            defer wg.Done()
            
            pred, err := m.Predict(ctx, horizon)
            if err != nil {
                log.Warn().
                    Err(err).
                    Str("model", m.Name()).
                    Msg("Model prediction failed")
                return
            }
            predCh <- pred
        }(model)
    }

    wg.Wait()
    close(predCh)

    for pred := range predCh {
        predictions = append(predictions, pred)
    }

    if len(predictions) == 0 {
        return nil, fmt.Errorf("no models produced predictions")
    }

    // # Apply ensemble strategy
    switch e.strategy {
    case StrategyWeightedAverage:
        return e.weightedAverage(predictions, horizon), nil
    case StrategyBestModel:
        return e.bestModel(predictions), nil
    case StrategyDynamicWeights:
        return e.dynamicWeightedAverage(predictions, horizon), nil
    default:
        return e.weightedAverage(predictions, horizon), nil
    }
}

// # Weighted average combination with confidence interval propagation
func (e *Ensemble) weightedAverage(predictions []*ForecastResult, horizon int) *ForecastResult {
    result := &ForecastResult{
        Timestamps:        make([]time.Time, horizon),
        PredictedValues:   make([]float64, horizon),
        LowerBound:        make([]float64, horizon),
        UpperBound:        make([]float64, horizon),
        ModelName:         "ensemble_weighted_average",
        ConfidenceInterval: 0.95,
        GeneratedAt:       time.Now(),
    }

    // # Generate future timestamps
    lastTimestamp := predictions[0].Timestamps[len(predictions[0].Timestamps)-1]
    for i := 0; i < horizon; i++ {
        result.Timestamps[i] = lastTimestamp.Add(time.Duration(i+1) * time.Hour)
    }

    // # Weighted combination for each time step
    for i := 0; i < horizon; i++ {
        var weightedSum float64
        var totalWeight float64
        var lowerSum float64
        var upperSum float64

        for _, pred := range predictions {
            if i < len(pred.PredictedValues) {
                weight := e.weights[pred.ModelName]
                weightedSum += pred.PredictedValues[i] * weight
                lowerSum += pred.LowerBound[i] * weight
                upperSum += pred.UpperBound[i] * weight
                totalWeight += weight
            }
        }

        if totalWeight > 0 {
            result.PredictedValues[i] = weightedSum / totalWeight
            result.LowerBound[i] = lowerSum / totalWeight
            result.UpperBound[i] = upperSum / totalWeight
        }
    }

    // # Calculate ensemble accuracy based on individual model accuracies
    var totalAccuracy float64
    for _, pred := range predictions {
        totalAccuracy += pred.Accuracy * e.weights[pred.ModelName]
    }
    result.Accuracy = totalAccuracy

    return result
}

// # Dynamic weight adjustment based on recent model performance
func (e *Ensemble) dynamicWeightedAverage(predictions []*ForecastResult, horizon int) *ForecastResult {
    // # Adjust weights based on recent accuracy trends
    e.adjustWeights(predictions)
    return e.weightedAverage(predictions, horizon)
}

// # Adjust model weights based on recent prediction accuracy
func (e *Ensemble) adjustWeights(predictions []*ForecastResult) {
    e.mu.Lock()
    defer e.mu.Unlock()

    var totalAccuracy float64
    accuracies := make(map[string]float64)

    for _, pred := range predictions {
        accuracies[pred.ModelName] = pred.Accuracy
        totalAccuracy += pred.Accuracy
    }

    // # Avoid division by zero
    if totalAccuracy == 0 {
        return
    }

    // # Assign weights proportional to accuracy with smoothing
    smoothingFactor := 0.7 // # Exponential smoothing factor
    for modelName, accuracy := range accuracies {
        newWeight := accuracy / totalAccuracy
        oldWeight := e.weights[modelName]
        
        // # Apply exponential smoothing to prevent drastic weight changes
        e.weights[modelName] = (smoothingFactor * newWeight) + 
                               ((1 - smoothingFactor) * oldWeight)
    }

    log.Debug().
        Interface("weights", e.weights).
        Msg("Model weights adjusted based on recent performance")
}

// # Best model strategy selects the model with highest accuracy
func (e *Ensemble) bestModel(predictions []*ForecastResult) *ForecastResult {
    var best *ForecastResult
    bestAccuracy := -1.0

    for _, pred := range predictions {
        if pred.Accuracy > bestAccuracy {
            bestAccuracy = pred.Accuracy
            best = pred
        }
    }

    best.ModelName = fmt.Sprintf("ensemble_best_%s", best.ModelName)
    return best
}

// # Validate input data for common issues in production time series
func (e *Ensemble) validateInputData(timestamps []time.Time, values []float64) error {
    // # Check for NaN or Inf values
    for i, v := range values {
        if math.IsNaN(v) || math.IsInf(v, 0) {
            return fmt.Errorf("invalid value at index %d: %f", i, v)
        }
    }

    // # Check timestamp ordering
    for i := 1; i < len(timestamps); i++ {
        if timestamps[i].Before(timestamps[i-1]) {
            return fmt.Errorf("timestamps not in chronological order at index %d", i)
        }
    }

    // # Check for anomalies that could indicate data quality issues
    mean, stddev := stat.MeanStdDev(values, nil)
    for i, v := range values {
        zscore := (v - mean) / stddev
        if math.Abs(zscore) > 5 { // # 5 sigma threshold
            log.Warn().
                Int("index", i).
                Float64("value", v).
                Float64("zscore", zscore).
                Msg("Potential anomaly detected in training data")
        }
    }

    return nil
}

// # Evaluate all models and update their performance metrics
func (e *Ensemble) EvaluateModels(actual []float64, predictions map[string][]float64) map[string]*ModelMetrics {
    results := make(map[string]*ModelMetrics)

    for modelName, predicted := range predictions {
        metrics := &ModelMetrics{
            ModelName: modelName,
            DataPointsEvaluated: len(actual),
            LastEvaluated: time.Now(),
        }

        // # Calculate error metrics
        if len(actual) == len(predicted) && len(actual) > 0 {
            // # MAPE calculation with protection against division by zero
            mapeSum := 0.0
            for i := range actual {
                if actual[i] != 0 {
                    mapeSum += math.Abs((actual[i] - predicted[i]) / actual[i])
                }
            }
            metrics.MAPE = (mapeSum / float64(len(actual))) * 100

            // # RMSE calculation
            var squaredErrors float64
            for i := range actual {
                error := actual[i] - predicted[i]
                squaredErrors += error * error
            }
            metrics.RMSE = math.Sqrt(squaredErrors / float64(len(actual)))

            // # MAE calculation
            var absErrors float64
            for i := range actual {
                absErrors += math.Abs(actual[i] - predicted[i])
            }
            metrics.MAE = absErrors / float64(len(actual))

            // # Recent accuracy (1 - MAPE/100) capped at 0
            metrics.RecentAccuracy = math.Max(0, 1 - metrics.MAPE/100)
        }

        results[modelName] = metrics
    }

    return results
}
