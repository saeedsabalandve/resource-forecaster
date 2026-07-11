# Resource Forecaster - Production-Grade Resource Prediction Microservice

![Build Status](https://github.com/your-org/resource-forecaster/workflows/CI/CD%20Pipeline/badge.svg)
![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)
![Docker Pulls](https://img.shields.io/docker/pulls/your-org/resource-forecaster)
![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)
![Coverage](https://codecov.io/gh/your-org/resource-forecaster/branch/main/graph/badge.svg)

## Overview

Resource Forecaster is a production-grade microservice that collects, stores, and analyzes server resource utilization metrics to predict future resource needs. It uses advanced ML models (ARIMA, Prophet, LSTM) in an ensemble approach to provide accurate forecasts with confidence intervals.

### Key Features

- 📊 **Real-time Metric Collection**: CPU, Memory, Disk, Network, GPU metrics
- 🔮 **ML-Powered Forecasting**: Ensemble of ARIMA, Prophet, and LSTM models
- 📈 **Confidence Intervals**: 95% prediction intervals with upper/lower bounds
- ☁️ **Cloud-Native**: Native AWS and Azure integrations
- 🚀 **Kubernetes Ready**: Production K8s manifests with HPA and PDB
- 🔒 **Security First**: JWT/OAuth2 auth, mTLS, API keys, rate limiting
- 📊 **Observability**: OpenTelemetry traces, Prometheus metrics, structured logging
- 🎯 **Auto-Scaling Advisory**: Proactive scaling recommendations
- 💾 **TimescaleDB**: Optimized time-series storage with compression and retention

## Architecture

┌─────────────────────────────────────────────────────────────┐
│                     Resource Forecaster                      │
├─────────────────────────────────────────────────────────────┤
│  ┌─────────────┐  ┌──────────────┐  ┌──────────────────┐  │
│  │  Collector   │  │  Forecaster   │  │  Auto-Scaling    │  │
│  │  (Metrics)   │  │  (ML Models)  │  │  Advisor         │  │
│  └──────┬──────┘  └──────┬───────┘  └────────┬─────────┘  │
│         │                │                    │             │
│  ┌──────┴────────────────┴────────────────────┴─────────┐  │
│  │                   TimescaleDB                         │  │
│  │              (Time-Series Storage)                    │  │
│  └──────────────────────────────────────────────────────┘  │
│                                                             │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────┐  │
│  │    Redis      │  │     S3        │  │  Prometheus      │  │
│  │  (Cache/RL)   │  │  (Models)     │  │  (Metrics)       │  │
│  └──────────────┘  └──────────────┘  └──────────────────┘  │
└─────────────────────────────────────────────────────────────┘

## Quick Start

### Prerequisites

- Go 1.21+
- Docker & Docker Compose
- TimescaleDB 2.11+
- Redis 7+

### Local Development

```bash
# Clone the repository
git clone https://github.com/your-org/resource-forecaster.git
cd resource-forecaster

# Copy environment configuration
cp .env.example .env
# Edit .env with your local settings

# Start dependencies
docker-compose -f deploy/docker/docker-compose.yml up -d

# Run database migrations
make db-migrate

# Start with hot reload (requires air)
make dev

# Or build and run manually
make build
./bin/resource-forecaster
```

Docker Deployment

```bash
# Build Docker image
make docker-build

# Run with Docker Compose
docker-compose -f deploy/docker/docker-compose.yml up -d

# Check logs
docker-compose logs -f resource-forecaster
```

Kubernetes Deployment

```bash
# Deploy to Kubernetes
make deploy-k8s

# Check deployment status
kubectl get pods -n monitoring -l app=resource-forecaster

# View logs
make logs

# Rollback if needed
make rollback
```

API Documentation

Generate Forecast

```bash
curl -X POST http://localhost:8080/api/v1/forecast \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{
    "hostname": "prod-web-01",
    "metricType": "cpu",
    "periodHours": 24,
    "confidenceInterval": 0.95
  }'
```

Ingest Metrics

```bash
curl -X POST http://localhost:8080/api/v1/metrics \
  -H "Content-Type: application/json" \
  -H "X-API-Key: $API_KEY" \
  -d '{
    "hostname": "prod-web-01",
    "timestamp": "2024-01-01T00:00:00Z",
    "cpu": {
      "usagePercent": 65.5,
      "numCores": 4
    },
    "memory": {
      "totalBytes": 17179869184,
      "usedBytes": 8589934592,
      "usedPercent": 50.0
    }
  }'
```

Configuration

All configuration is done through environment variables or config files. See .env.example for a complete list of options.

Critical Settings

Variable Description Default
TSDB_HOST TimescaleDB host localhost
TSDB_DATABASE Database name resource_forecaster
AUTH_JWT_SECRET JWT signing secret (min 256-bit) -
TELEMETRY_TRACING_SAMPLE_RATE Tracing sample rate (0.0-1.0) 0.1
SCHEDULER_TRAINING_INTERVAL Model retraining interval 6h

Monitoring

Prometheus Metrics

The service exposes Prometheus metrics on port 9090:

· http_requests_total - HTTP request counter
· forecast_generation_duration_seconds - Forecast generation time
· resource_metrics_collected_total - Metrics collected counter
· rate_limited_requests_total - Rate limited requests

Grafana Dashboards

Import the provided Grafana dashboards from deploy/grafana/dashboards/ for:

· Service Overview
· Forecasting Accuracy
· Resource Utilization Trends
· API Performance

Health Checks

```bash
# Liveness
curl http://localhost:8080/api/v1/health/live

# Readiness
curl http://localhost:8080/api/v1/health/ready

# Full health check
curl http://localhost:8080/api/v1/health
```

Production Considerations

Security

· Always use TLS/SSL for database connections
· Rotate JWT secrets regularly
· Use AWS Secrets Manager or Azure Key Vault for sensitive values
· Enable mTLS between services
· Regular security scanning with Trivy/Grype

Performance

· Use connection pooling for database connections
· Enable TimescaleDB compression for historical data
· Configure appropriate retention policies
· Use Redis caching for frequent queries
· Scale horizontally with Kubernetes HPA

Reliability

· Deploy with at least 3 replicas
· Configure PodDisruptionBudget
· Use multiple availability zones
· Regular backups of TimescaleDB
· Monitor with proper alerting thresholds

Testing

```bash
# Unit tests
make test

# Integration tests (requires Docker)
make test-integration

# Load tests
make test-load

# Security scan
make security-scan
```

Variable Description Default
TSDB_HOST TimescaleDB host localhost
TSDB_DATABASE Database name resource_forecaster
AUTH_JWT_SECRET JWT signing secret (min 256-bit) -
TELEMETRY_TRACING_SAMPLE_RATE Tracing sample rate (0.0-1.0) 0.1
SCHEDULER_TRAINING_INTERVAL Model retraining interval 6h
