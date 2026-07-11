package aws

import (
    "context"
    "fmt"
    "time"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/service/autoscaling"
    "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
    "github.com/aws/aws-sdk-go-v2/service/cloudwatch"
    cwTypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
    "github.com/rs/zerolog/log"

    "resource-forecaster/internal/forecaster/models"
)

// # AutoScalingAdvisor provides proactive scaling recommendations
// # Based on forecasted resource utilization patterns
type AutoScalingAdvisor struct {
    asgClient     *autoscaling.Client
    cwClient      *cloudwatch.Client
    forecaster    *models.Ensemble
}

// # ScalingRecommendation provides right-sizing suggestions
type ScalingRecommendation struct {
    ASGName              string
    CurrentDesired       int32
    CurrentMin           int32
    CurrentMax           int32
    RecommendedMin       int32
    RecommendedMax       int32
    RecommendedDesired   int32
    ForecastedCPUAvg     float64
    ForecastedCPUPeak    float64
    ConfidenceScore      float64
    CostImpactMonthly    float64  // # Estimated monthly cost savings
    Reasoning            string
}

// # NewAutoScalingAdvisor creates advisor with AWS SDK v2 clients
func NewAutoScalingAdvisor(cfg aws.Config, forecaster *models.Ensemble) *AutoScalingAdvisor {
    return &AutoScalingAdvisor{
        asgClient:  autoscaling.NewFromConfig(cfg),
        cwClient:   cloudwatch.NewFromConfig(cfg),
        forecaster: forecaster,
    }
}

// # AnalyzeASG provides comprehensive scaling analysis with forecasts
func (a *AutoScalingAdvisor) AnalyzeASG(
    ctx context.Context, 
    asgName string, 
    lookbackHours int,
    forecastHours int,
) (*ScalingRecommendation, error) {
    
    // # Describe current ASG configuration
    asgOutput, err := a.asgClient.DescribeAutoScalingGroups(ctx, 
        &autoscaling.DescribeAutoScalingGroupsInput{
            AutoScalingGroupNames: []string{asgName},
        },
    )
    if err != nil {
        return nil, fmt.Errorf("failed to describe ASG: %w", err)
    }

    if len(asgOutput.AutoScalingGroups) == 0 {
        return nil, fmt.Errorf("ASG %s not found", asgName)
    }

    asg := asgOutput.AutoScalingGroups[0]
    
    // # Fetch historical CPU utilization from CloudWatch
    endTime := time.Now()
    startTime := endTime.Add(-time.Duration(lookbackHours) * time.Hour)
    
    metricsOutput, err := a.cwClient.GetMetricStatistics(ctx,
        &cloudwatch.GetMetricStatisticsInput{
            Namespace:  aws.String("AWS/EC2"),
            MetricName: aws.String("CPUUtilization"),
            Dimensions: []cwTypes.Dimension{
                {
                    Name:  aws.String("AutoScalingGroupName"),
                    Value: aws.String(asgName),
                },
            },
            StartTime:  aws.Time(startTime),
            EndTime:    aws.Time(endTime),
            Period:     aws.Int32(3600), // # 1-hour granularity
            Statistics: []cwTypes.Statistic{
                cwTypes.StatisticAverage,
                cwTypes.StatisticMaximum,
            },
            Unit: cwTypes.StandardUnitPercent,
        },
    )
    if err != nil {
        return nil, fmt.Errorf("failed to get CloudWatch metrics: %w", err)
    }

    // # Convert CloudWatch datapoints to time series for forecasting
    timestamps := make([]time.Time, len(metricsOutput.Datapoints))
    values := make([]float64, len(metricsOutput.Datapoints))
    
    for i, dp := range metricsOutput.Datapoints {
        timestamps[i] = *dp.Timestamp
        values[i] = *dp.Average
    }

    // # Train forecasting model on historical data
    if err := a.forecaster.TrainAll(ctx, timestamps, values); err != nil {
        return nil, fmt.Errorf("failed to train forecasting models: %w", err)
    }

    // # Generate forecast for specified horizon
    forecast, err := a.forecaster.Predict(ctx, forecastHours)
    if err != nil {
        return nil, fmt.Errorf("failed to generate forecast: %w", err)
    }

    // # Analyze forecast and generate recommendations
    recommendation := a.generateRecommendation(asg, forecast)
    
    // # Calculate cost impact using AWS pricing API
    recommendation.CostImpactMonthly = a.calculateCostImpact(asg, recommendation)

    return recommendation, nil
}

// # Generate scaling recommendations based on forecasted metrics
func (a *AutoScalingAdvisor) generateRecommendation(
    asg types.AutoScalingGroup, 
    forecast *models.ForecastResult,
) *ScalingRecommendation {
    
    rec := &ScalingRecommendation{
        ASGName:            *asg.AutoScalingGroupName,
        CurrentDesired:     *asg.DesiredCapacity,
        CurrentMin:         *asg.MinSize,
        CurrentMax:         *asg.MaxSize,
        ConfidenceScore:    forecast.Accuracy,
    }

    // # Calculate forecast statistics
    var sum, max float64
    for _, v := range forecast.PredictedValues {
        sum += v
        if v > max {
            max = v
        }
    }
    rec.ForecastedCPUAvg = sum / float64(len(forecast.PredictedValues))
    rec.ForecastedCPUPeak = max

    // # Generate right-sizing recommendations
    // # Target 60-70% average utilization for optimal performance/cost
    targetUtilization := 65.0
    
    // # Calculate recommended desired capacity
    desiredCapacity := int32(math.Ceil(
        (rec.ForecastedCPUAvg / targetUtilization) * float64(*asg.DesiredCapacity),
    ))
    rec.RecommendedDesired = maxInt32(desiredCapacity, 1)

    // # Calculate recommended min based on forecast minimum + buffer
    minForecast := forecast.PredictedValues[0]
    for _, v := range forecast.PredictedValues {
        if v < minForecast {
            minForecast = v
        }
    }
    minCapacity := int32(math.Ceil(
        (minForecast / targetUtilization) * float64(*asg.MinSize),
    ))
    rec.RecommendedMin = maxInt32(minCapacity, 1)

    // # Calculate recommended max based on forecast peak + 20% buffer
    maxCapacity := int32(math.Ceil(
        (rec.ForecastedCPUPeak / targetUtilization) * float64(*asg.MaxSize) * 1.2,
    ))
    rec.RecommendedMax = maxInt32(maxCapacity, rec.RecommendedMin)

    // # Generate human-readable reasoning
    rec.Reasoning = fmt.Sprintf(
        "Based on %d-hour forecast with %.1f%% confidence: "+
        "Average CPU utilization expected at %.1f%% (peak %.1f%%). "+
        "Recommended scaling to maintain %.0f%% target utilization. "+
        "Min: %d (was %d), Max: %d (was %d), Desired: %d (was %d)",
        len(forecast.PredictedValues),
        rec.ConfidenceScore*100,
        rec.ForecastedCPUAvg,
        rec.ForecastedCPUPeak,
        targetUtilization,
        rec.RecommendedMin, rec.CurrentMin,
        rec.RecommendedMax, rec.CurrentMax,
        rec.RecommendedDesired, rec.CurrentDesired,
    )

    return rec
}

// # Calculate monthly cost impact of scaling changes
func (a *AutoScalingAdvisor) calculateCostImpact(
    asg types.AutoScalingGroup, 
    rec *ScalingRecommendation,
) float64 {
    // # Simplified cost calculation - in production would use AWS Pricing API
    // # and consider reserved instances, savings plans, etc.
    hoursPerMonth := 730.0
    hourlyCostPerInstance := 0.10 // # Example cost, should be from pricing API
    
    currentMonthlyCost := float64(rec.CurrentDesired) * hourlyCostPerInstance * hoursPerMonth
    recommendedMonthlyCost := float64(rec.RecommendedDesired) * hourlyCostPerInstance * hoursPerMonth
    
    return currentMonthlyCost - recommendedMonthlyCost
}
