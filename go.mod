module resource-forecaster

go 1.21

require (
    github.com/aws/aws-sdk-go-v2 v1.24.0
    github.com/aws/aws-sdk-go-v2/config v1.26.1
    github.com/aws/aws-sdk-go-v2/service/autoscaling v1.35.0
    github.com/aws/aws-sdk-go-v2/service/cloudwatch v1.32.0
    github.com/aws/aws-sdk-go-v2/service/ec2 v1.141.0
    github.com/aws/aws-sdk-go-v2/service/s3 v1.48.0
    github.com/aws/aws-sdk-go-v2/service/secretsmanager v1.26.0
    github.com/Azure/azure-sdk-for-go/sdk/azcore v1.9.0
    github.com/Azure/azure-sdk-for-go/sdk/azidentity v1.4.0
    github.com/Azure/azure-sdk-for-go/sdk/monitor/azquery v1.0.0
    github.com/go-chi/chi/v5 v5.0.10
    github.com/go-chi/cors v1.2.1
    github.com/go-chi/httprate v0.7.4
    github.com/golang-jwt/jwt/v5 v5.2.0
    github.com/google/uuid v1.5.0
    github.com/jackc/pgx/v5 v5.5.1
    github.com/prometheus/client_golang v1.17.0
    github.com/redis/go-redis/v9 v9.3.1
    github.com/robfig/cron/v3 v3.0.1
    github.com/rs/zerolog v1.31.0
    github.com/spf13/viper v1.18.1
    go.opentelemetry.io/otel v1.21.0
    go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.21.0
    go.opentelemetry.io/otel/metric v1.21.0
    go.opentelemetry.io/otel/sdk v1.21.0
    go.opentelemetry.io/otel/trace v1.21.0
    go.uber.org/automaxprocs v1.5.3
    golang.org/x/time v0.5.0
    gonum.org/v1/gonum v0.14.0
    gopkg.in/natefinch/lumberjack.v2 v2.2.1
)

require (
    // # Indirect dependencies would be listed here
    // # This is a simplified version for the example
)
