package config

import (
    "fmt"
    "os"
    "time"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/config"
    "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
    "github.com/spf13/viper"
)

// # Production-grade configuration with support for multiple sources:
// # environment variables, config files, AWS Secrets Manager, Azure Key Vault
type Config struct {
    Server      ServerConfig
    TimescaleDB TimescaleDBConfig
    Redis       RedisConfig
    S3          S3Config
    Telemetry   TelemetryConfig
    Scheduler   SchedulerConfig
    Auth        AuthConfig
    Cloud       CloudConfig
    Forecaster  ForecasterConfig
}

type ServerConfig struct {
    Port            int           `mapstructure:"SERVER_PORT"`
    ReadTimeout     time.Duration `mapstructure:"SERVER_READ_TIMEOUT"`
    WriteTimeout    time.Duration `mapstructure:"SERVER_WRITE_TIMEOUT"`
    MaxRequestBody  int64         `mapstructure:"SERVER_MAX_REQUEST_BODY"`
    CORSOrigins     []string      `mapstructure:"SERVER_CORS_ORIGINS"`
}

type TimescaleDBConfig struct {
    Host            string        `mapstructure:"TSDB_HOST"`
    Port            int           `mapstructure:"TSDB_PORT"`
    Database        string        `mapstructure:"TSDB_DATABASE"`
    Username        string        `mapstructure:"TSDB_USERNAME"`
    Password        string        `mapstructure:"TSDB_PASSWORD"`
    MaxConnections  int           `mapstructure:"TSDB_MAX_CONNECTIONS"`
    MinConnections  int           `mapstructure:"TSDB_MIN_CONNECTIONS"`
    MaxConnLifetime time.Duration `mapstructure:"TSDB_MAX_CONN_LIFETIME"`
    SSLMode         string        `mapstructure:"TSDB_SSL_MODE"`
    RetentionDays   int           `mapstructure:"TSDB_RETENTION_DAYS"`
}

type TelemetryConfig struct {
    MetricsPort       int    `mapstructure:"TELEMETRY_METRICS_PORT"`
    TracingEndpoint   string `mapstructure:"TELEMETRY_TRACING_ENDPOINT"`
    TracingSampleRate float64 `mapstructure:"TELEMETRY_TRACING_SAMPLE_RATE"`
    LogLevel          string `mapstructure:"TELEMETRY_LOG_LEVEL"`
    ServiceName       string `mapstructure:"TELEMETRY_SERVICE_NAME"`
    Environment       string `mapstructure:"TELEMETRY_ENVIRONMENT"`
}

type SchedulerConfig struct {
    CollectionInterval  time.Duration `mapstructure:"SCHEDULER_COLLECTION_INTERVAL"`
    TrainingInterval    time.Duration `mapstructure:"SCHEDULER_TRAINING_INTERVAL"`
    EvaluationInterval  time.Duration `mapstructure:"SCHEDULER_EVALUATION_INTERVAL"`
    ForecastHorizonHours int         `mapstructure:"SCHEDULER_FORECAST_HORIZON_HOURS"`
    RetentionPeriodDays  int         `mapstructure:"SCHEDULER_RETENTION_PERIOD_DAYS"`
}

type AuthConfig struct {
    JWTSecret          string        `mapstructure:"AUTH_JWT_SECRET"`
    JWTExpiration      time.Duration `mapstructure:"AUTH_JWT_EXPIRATION"`
    APIKeyHeader       string        `mapstructure:"AUTH_API_KEY_HEADER"`
    InternalAPIKeys    []string      `mapstructure:"AUTH_INTERNAL_API_KEYS"`
    OAuth2IssuerURL    string        `mapstructure:"AUTH_OAUTH2_ISSUER_URL"`
}

type CloudConfig struct {
    Provider          string `mapstructure:"CLOUD_PROVIDER"` // # aws, azure, gcp
    Region            string `mapstructure:"CLOUD_REGION"`
    AWSAccessKeyID    string `mapstructure:"AWS_ACCESS_KEY_ID"`
    AWSSecretAccessKey string `mapstructure:"AWS_SECRET_ACCESS_KEY"`
    AzureTenantID     string `mapstructure:"AZURE_TENANT_ID"`
    AzureClientID     string `mapstructure:"AZURE_CLIENT_ID"`
    AzureClientSecret string `mapstructure:"AZURE_CLIENT_SECRET"`
}

type ForecasterConfig struct {
    ModelStorageBucket   string  `mapstructure:"FORECASTER_MODEL_BUCKET"`
    MinDataPoints        int     `mapstructure:"FORECASTER_MIN_DATA_POINTS"`
    ConfidenceInterval   float64 `mapstructure:"FORECASTER_CONFIDENCE_INTERVAL"`
    EnsembleWeightsUpdate bool   `mapstructure:"FORECASTER_ENSEMBLE_WEIGHTS_UPDATE"`
    GPUEnabled           bool    `mapstructure:"FORECASTER_GPU_ENABLED"`
}

// # Load configuration from multiple sources with precedence:
// # 1. Environment variables (highest)
// # 2. Configuration file
// # 3. Cloud Secrets Manager (lowest, but encrypted)
func Load() (*Config, error) {
    v := viper.New()

    // # Set defaults for all configuration values
    setDefaults(v)

    // # Read from config file if exists
    v.SetConfigName("config")
    v.SetConfigType("yaml")
    v.AddConfigPath("/etc/resource-forecaster/")
    v.AddConfigPath("$HOME/.resource-forecaster/")
    v.AddConfigPath(".")

    if err := v.ReadInConfig(); err != nil {
        if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
            return nil, fmt.Errorf("error reading config file: %w", err)
        }
    }

    // # Override with environment variables
    v.AutomaticEnv()

    // # Fetch secrets from AWS Secrets Manager or Azure Key Vault if enabled
    if v.GetString("CLOUD_PROVIDER") == "aws" {
        if err := loadAWSSecrets(v); err != nil {
            return nil, fmt.Errorf("failed to load AWS secrets: %w", err)
        }
    } else if v.GetString("CLOUD_PROVIDER") == "azure" {
        if err := loadAzureSecrets(v); err != nil {
            return nil, fmt.Errorf("failed to load Azure secrets: %w", err)
        }
    }

    var cfg Config
    if err := v.Unmarshal(&cfg); err != nil {
        return nil, fmt.Errorf("failed to unmarshal config: %w", err)
    }

    // # Validate required configuration
    if err := cfg.validate(); err != nil {
        return nil, err
    }

    return &cfg, nil
}

// # Set production-safe defaults
func setDefaults(v *viper.Viper) {
    v.SetDefault("SERVER_PORT", 8080)
    v.SetDefault("SERVER_READ_TIMEOUT", "15s")
    v.SetDefault("SERVER_WRITE_TIMEOUT", "30s")
    v.SetDefault("SERVER_MAX_REQUEST_BODY", 1048576) // # 1MB
    v.SetDefault("TSDB_PORT", 5432)
    v.SetDefault("TSDB_MAX_CONNECTIONS", 100)
    v.SetDefault("TSDB_MIN_CONNECTIONS", 10)
    v.SetDefault("TSDB_SSL_MODE", "require")
    v.SetDefault("TSDB_RETENTION_DAYS", 90)
    v.SetDefault("TELEMETRY_METRICS_PORT", 9090)
    v.SetDefault("TELEMETRY_TRACING_SAMPLE_RATE", 0.1) // # 10% sampling for tracing
    v.SetDefault("TELEMETRY_LOG_LEVEL", "info")
    v.SetDefault("TELEMETRY_ENVIRONMENT", "production")
    v.SetDefault("SCHEDULER_COLLECTION_INTERVAL", "60s")
    v.SetDefault("SCHEDULER_TRAINING_INTERVAL", "6h")
    v.SetDefault("SCHEDULER_EVALUATION_INTERVAL", "24h")
    v.SetDefault("SCHEDULER_FORECAST_HORIZON_HOURS", 168) // # 1 week
    v.SetDefault("SCHEDULER_RETENTION_PERIOD_DAYS", 365)
    v.SetDefault("FORECASTER_MIN_DATA_POINTS", 168) // # Need at least 1 week of hourly data
    v.SetDefault("FORECASTER_CONFIDENCE_INTERVAL", 0.95)
    v.SetDefault("FORECASTER_ENSEMBLE_WEIGHTS_UPDATE", true)
    v.SetDefault("FORECASTER_GPU_ENABLED", false)
}

// # Load secrets from AWS Secrets Manager
func loadAWSSecrets(v *viper.Viper) error {
    cfg, err := config.LoadDefaultConfig(context.Background(),
        config.WithRegion(v.GetString("CLOUD_REGION")),
    )
    if err != nil {
        return fmt.Errorf("failed to load AWS config: %w", err)
    }

    client := secretsmanager.NewFromConfig(cfg)
    secretID := v.GetString("AWS_SECRET_ID")
    
    input := &secretsmanager.GetSecretValueInput{
        SecretId: aws.String(secretID),
    }

    result, err := client.GetSecretValue(context.Background(), input)
    if err != nil {
        return fmt.Errorf("failed to get secret value: %w", err)
    }

    // # Parse JSON secret and inject into viper
    var secrets map[string]string
    if err := json.Unmarshal([]byte(*result.SecretString), &secrets); err != nil {
        return fmt.Errorf("failed to unmarshal secret: %w", err)
    }

    for key, value := range secrets {
        v.Set(key, value)
    }

    return nil
}

// # Validate configuration for production readiness
func (c *Config) validate() error {
    if c.TimescaleDB.Host == "" {
        return fmt.Errorf("timescaleDB host is required")
    }
    if c.Server.Port <= 0 || c.Server.Port > 65535 {
        return fmt.Errorf("invalid server port: %d", c.Server.Port)
    }
    if c.Forecaster.MinDataPoints < 24 {
        return fmt.Errorf("minimum data points must be at least 24 for reliable forecasting")
    }
    if c.Scheduler.CollectionInterval < 10*time.Second {
        return fmt.Errorf("collection interval too aggressive (minimum 10s)")
    }
    return nil
}
