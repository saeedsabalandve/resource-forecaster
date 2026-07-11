# # Terraform configuration for resource-forecaster infrastructure
# # Multi-cloud deployment supporting AWS and Azure

terraform {
  required_version = ">= 1.5.0"
  
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
    
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 3.0"
    }
    
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.25"
    }
    
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.12"
    }
    
    random = {
      source  = "hashicorp/random"
      version = "~> 3.5"
    }
    
    vault = {
      source  = "hashicorp/vault"
      version = "~> 3.25"
    }
  }
  
  backend "s3" {
    bucket         = "resource-forecaster-terraform-state"
    key            = "production/terraform.tfstate"
    region         = "us-east-1"
    encrypt        = true
    dynamodb_table = "terraform-state-lock"
  }
}

# # Data sources for current AWS account and region
data "aws_caller_identity" "current" {}
data "aws_region" "current" {}
data "aws_availability_zones" "available" {
  state = "available"
}

# # Random suffix for unique resource naming
resource "random_string" "suffix" {
  length  = 8
  special = false
  upper   = false
}

# # Local variables
locals {
  name_prefix = "forecaster-${var.environment}"
  
  common_tags = {
    Environment = var.environment
    Application = "resource-forecaster"
    ManagedBy   = "terraform"
    Team        = "platform-engineering"
    CostCenter  = var.cost_center
    Compliance  = "sox-pci"
  }
}

# # VPC Module for network infrastructure
module "vpc" {
  source = "terraform-aws-modules/vpc/aws"
  version = "5.5.1"

  name = "${local.name_prefix}-vpc"
  cidr = var.vpc_cidr

  azs             = slice(data.aws_availability_zones.available.names, 0, 3)
  private_subnets = var.private_subnet_cidrs
  public_subnets  = var.public_subnet_cidrs

  enable_nat_gateway     = true
  single_nat_gateway     = var.environment != "production"
  one_nat_gateway_per_az = var.environment == "production"
  
  enable_dns_hostnames = true
  enable_dns_support   = true

  enable_flow_log                      = true
  create_flow_log_cloudwatch_log_group = true
  create_flow_log_cloudwatch_iam_role  = true
  flow_log_max_aggregation_interval    = 60

  tags = local.common_tags
}

# # EKS Cluster for running the microservice
module "eks" {
  source = "terraform-aws-modules/eks/aws"
  version = "19.21.0"

  cluster_name    = "${local.name_prefix}-cluster"
  cluster_version = var.kubernetes_version

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.private_subnets

  cluster_endpoint_private_access = true
  cluster_endpoint_public_access  = true

  enable_irsa = true

  # # Cluster security
  cluster_encryption_config = {
    provider_key_arn = aws_kms_key.eks.arn
    resources        = ["secrets"]
  }

  # # Node groups for different workload types
  node_security_group_additional_rules = {
    ingress_self_all = {
      description = "Node to node all ports/protocols"
      protocol    = "-1"
      from_port   = 0
      to_port     = 0
      type        = "ingress"
      self        = true
    }
    egress_all = {
      description      = "Node all egress"
      protocol         = "-1"
      from_port        = 0
      to_port          = 0
      type             = "egress"
      cidr_blocks      = ["0.0.0.0/0"]
      ipv6_cidr_blocks = ["::/0"]
    }
  }

  eks_managed_node_groups = {
    # # General purpose workloads
    general = {
      name           = "${local.name_prefix}-general"
      instance_types = var.general_instance_types
      
      desired_size = var.general_desired_size
      min_size     = var.general_min_size
      max_size     = var.general_max_size

      capacity_type = "ON_DEMAND"
      
      disk_size = 100
      disk_type = "gp3"
      
      create_launch_template = true
      launch_template_os     = "bottlerocket"

      update_config = {
        max_unavailable_percentage = 25
      }

      tags = merge(local.common_tags, {
        WorkloadType = "general"
      })
    }

    # # Compute optimized for ML/forecasting workloads
    compute = {
      name           = "${local.name_prefix}-compute"
      instance_types = var.compute_instance_types
      
      desired_size = var.compute_desired_size
      min_size     = var.compute_min_size
      max_size     = var.compute_max_size

      capacity_type = "ON_DEMAND"
      
      disk_size = 200
      disk_type = "gp3"
      
      create_launch_template = true
      launch_template_os     = "bottlerocket"

      k8s_labels = {
        workload = "forecasting"
        gpu      = "false"
      }

      taints = [
        {
          key    = "workload"
          value  = "forecasting"
          effect = "NO_SCHEDULE"
        }
      ]

      update_config = {
        max_unavailable_percentage = 25
      }

      tags = merge(local.common_tags, {
        WorkloadType = "compute"
      })
    }

    # # GPU-enabled nodes for LSTM training
    gpu = {
      name           = "${local.name_prefix}-gpu"
      instance_types = var.gpu_instance_types
      
      desired_size = var.gpu_desired_size
      min_size     = var.gpu_min_size
      max_size     = var.gpu_max_size

      capacity_type = "ON_DEMAND"
      
      disk_size = 200
      disk_type = "gp3"
      
      create_launch_template = true
      launch_template_os     = "bottlerocket"

      k8s_labels = {
        workload = "forecasting"
        gpu      = "true"
      }

      taints = [
        {
          key    = "nvidia.com/gpu"
          value  = "true"
          effect = "NO_SCHEDULE"
        }
      ]

      update_config = {
        max_unavailable_percentage = 25
      }

      tags = merge(local.common_tags, {
        WorkloadType = "gpu"
      })
    }
  }

  # # Install AWS Load Balancer Controller
  cluster_addons = {
    coredns = {
      most_recent = true
    }
    kube-proxy = {
      most_recent = true
    }
    vpc-cni = {
      most_recent = true
    }
    aws-ebs-csi-driver = {
      most_recent = true
    }
  }

  tags = local.common_tags
}

# # KMS key for EKS encryption
resource "aws_kms_key" "eks" {
  description             = "EKS Secret Encryption Key for ${local.name_prefix}"
  deletion_window_in_days = 30
  enable_key_rotation     = true
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "Enable IAM User Permissions"
        Effect = "Allow"
        Principal = {
          AWS = "arn:aws:iam::${data.aws_caller_identity.current.account_id}:root"
        }
        Action   = "kms:*"
        Resource = "*"
      },
    ]
  })

  tags = local.common_tags
}

# # TimescaleDB RDS instance for metric storage
module "timescaledb" {
  source = "terraform-aws-modules/rds/aws"
  version = "6.1.1"

  identifier = "${local.name_prefix}-timescaledb"

  engine               = "postgres"
  engine_version       = "15.4"
  family               = "postgres15"
  major_engine_version = "15"
  instance_class       = var.timescaledb_instance_class

  allocated_storage     = var.timescaledb_storage_gb
  max_allocated_storage = var.timescaledb_max_storage_gb
  storage_encrypted     = true
  storage_type          = "gp3"
  iops                  = 3000

  db_name  = var.timescaledb_name
  username = var.timescaledb_username
  port     = 5432

  manage_master_user_password = true

  multi_az               = var.environment == "production"
  db_subnet_group_name   = module.vpc.database_subnet_group
  vpc_security_group_ids = [aws_security_group.timescaledb.id]

  maintenance_window      = "Mon:00:00-Mon:03:00"
  backup_window           = "03:00-06:00"
  backup_retention_period = var.environment == "production" ? 30 : 7

  enabled_cloudwatch_logs_exports = ["postgresql", "upgrade"]
  create_cloudwatch_log_group     = true

  performance_insights_enabled          = true
  performance_insights_retention_period = 7
  create_monitoring_role                = true
  monitoring_interval                   = 60

  deletion_protection = var.environment == "production"
  skip_final_snapshot = var.environment != "production"
  
  apply_immediately = var.environment != "production"

  parameters = [
    {
      name  = "shared_preload_libraries"
      value = "timescaledb,pg_stat_statements"
    },
    {
      name  = "max_connections"
      value = var.timescaledb_max_connections
    },
    {
      name  = "max_worker_processes"
      value = "16"
    },
    {
      name  = "max_parallel_workers"
      value = "8"
    },
    {
      name  = "max_parallel_workers_per_gather"
      value = "4"
    },
    {
      name  = "work_mem"
      value = "65536"
    },
    {
      name  = "maintenance_work_mem"
      value = "2097152"
    },
    {
      name  = "effective_cache_size"
      value = "22548578304"
    },
    {
      name  = "random_page_cost"
      value = "1.1"
    },
    {
      name  = "effective_io_concurrency"
      value = "200"
    },
    {
      name  = "autovacuum"
      value = "1"
    },
    {
      name  = "autovacuum_max_workers"
      value = "5"
    },
    {
      name  = "autovacuum_naptime"
      value = "30"
    },
  ]

  tags = local.common_tags
}

# # ElastiCache Redis for rate limiting and caching
resource "aws_elasticache_cluster" "redis" {
  cluster_id           = "${local.name_prefix}-redis"
  engine               = "redis"
  node_type            = var.redis_node_type
  num_cache_nodes      = var.environment == "production" ? 2 : 1
  parameter_group_name = aws_elasticache_parameter_group.redis.name
  engine_version       = "7.1"
  port                 = 6379
  subnet_group_name    = aws_elasticache_subnet_group.redis.name
  security_group_ids   = [aws_security_group.redis.id]
  
  snapshot_window          = "06:00-08:00"
  snapshot_retention_limit = var.environment == "production" ? 7 : 0
  
  automatic_failover_enabled = var.environment == "production"
  multi_az_enabled          = var.environment == "production"
  
  transit_encryption_enabled = true
  
  tags = local.common_tags
}

resource "aws_elasticache_parameter_group" "redis" {
  family = "redis7"
  name   = "${local.name_prefix}-redis-params"

  parameter {
    name  = "maxmemory-policy"
    value = "volatile-lru"
  }
  
  parameter {
    name  = "timeout"
    value = "300"
  }
  
  parameter {
    name  = "tcp-keepalive"
    value = "300"
  }
  
  parameter {
    name  = "maxclients"
    value = "65000"
  }
}

resource "aws_elasticache_subnet_group" "redis" {
  name       = "${local.name_prefix}-redis-subnet"
  subnet_ids = module.vpc.private_subnets
}

# # S3 bucket for model storage
resource "aws_s3_bucket" "models" {
  bucket = "${local.name_prefix}-models-${random_string.suffix.result}"
  
  force_destroy = var.environment != "production"
  
  tags = local.common_tags
}

resource "aws_s3_bucket_versioning" "models" {
  bucket = aws_s3_bucket.models.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "models" {
  bucket = aws_s3_bucket.models.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.s3.arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "models" {
  bucket = aws_s3_bucket.models.id

  rule {
    id     = "model_versions"
    status = "Enabled"

    noncurrent_version_expiration {
      noncurrent_days = 90
    }
    
    noncurrent_version_transition {
      noncurrent_days = 30
      storage_class   = "STANDARD_IA"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "models" {
  bucket = aws_s3_bucket.models.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# # KMS key for S3 encryption
resource "aws_kms_key" "s3" {
  description             = "S3 encryption key for ${local.name_prefix}"
  deletion_window_in_days = 30
  enable_key_rotation     = true

  tags = local.common_tags
}

# # Security groups
resource "aws_security_group" "timescaledb" {
  name        = "${local.name_prefix}-timescaledb"
  description = "Security group for TimescaleDB"
  vpc_id      = module.vpc.vpc_id

  ingress {
    description     = "PostgreSQL from EKS"
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [module.eks.cluster_security_group_id]
  }

  tags = local.common_tags
}

resource "aws_security_group" "redis" {
  name        = "${local.name_prefix}-redis"
  description = "Security group for Redis"
  vpc_id      = module.vpc.vpc_id

  ingress {
    description     = "Redis from EKS"
    from_port       = 6379
    to_port         = 6379
    protocol        = "tcp"
    security_groups = [module.eks.cluster_security_group_id]
  }

  tags = local.common_tags
}

# # IAM roles for service accounts (IRSA)
module "forecaster_irsa" {
  source = "terraform-aws-modules/iam/aws//modules/iam-role-for-service-accounts-eks"

  role_name = "${local.name_prefix}-sa-role"
  
  role_policy_arns = {
    s3_models = aws_iam_policy.s3_models_access.arn
  }

  oidc_providers = {
    main = {
      provider_arn               = module.eks.oidc_provider_arn
      namespace_service_accounts = ["monitoring:resource-forecaster"]
    }
  }

  tags = local.common_tags
}

resource "aws_iam_policy" "s3_models_access" {
  name        = "${local.name_prefix}-s3-models-access"
  description = "Policy for accessing S3 model storage"

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "s3:GetObject",
          "s3:PutObject",
          "s3:ListBucket",
          "s3:DeleteObject"
        ]
        Resource = [
          aws_s3_bucket.models.arn,
          "${aws_s3_bucket.models.arn}/*"
        ]
      }
    ]
  })
}

# # CloudWatch Log Groups
resource "aws_cloudwatch_log_group" "forecaster" {
  name              = "/aws/eks/${local.name_prefix}/resource-forecaster"
  retention_in_days = var.log_retention_days
  
  kms_key_id = aws_kms_key.cloudwatch.arn

  tags = local.common_tags
}

resource "aws_kms_key" "cloudwatch" {
  description             = "CloudWatch Log encryption key"
  deletion_window_in_days = 30
  enable_key_rotation     = true

  tags = local.common_tags
}

# # CloudWatch Alarms
resource "aws_cloudwatch_metric_alarm" "forecast_errors" {
  alarm_name          = "${local.name_prefix}-forecast-errors"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = "2"
  metric_name         = "forecast_errors_total"
  namespace           = "resource-forecaster"
  period              = "300"
  statistic           = "Sum"
  threshold           = "10"
  alarm_description   = "Forecast errors exceeded threshold"
  
  alarm_actions = [aws_sns_topic.alerts.arn]
  ok_actions    = [aws_sns_topic.alerts.arn]

  tags = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "high_cpu" {
  alarm_name          = "${local.name_prefix}-high-cpu"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = "2"
  metric_name         = "cpu_utilization"
  namespace           = "AWS/ECS"
  period              = "300"
  statistic           = "Average"
  threshold           = "85"
  alarm_description   = "CPU utilization too high"
  
  alarm_actions = [aws_sns_topic.alerts.arn]

  tags = local.common_tags
}

# # SNS for alerts
resource "aws_sns_topic" "alerts" {
  name = "${local.name_prefix}-alerts"
  
  kms_master_key_id = aws_kms_key.sns.arn
  
  tags = local.common_tags
}

resource "aws_sns_topic_subscription" "email" {
  topic_arn = aws_sns_topic.alerts.arn
  protocol  = "email"
  endpoint  = var.alert_email
}

resource "aws_kms_key" "sns" {
  description             = "SNS encryption key"
  deletion_window_in_days = 30
  enable_key_rotation     = true

  tags = local.common_tags
}

# # Outputs
output "eks_cluster_endpoint" {
  description = "EKS cluster endpoint"
  value       = module.eks.cluster_endpoint
}

output "eks_cluster_name" {
  description = "EKS cluster name"
  value       = module.eks.cluster_name
}

output "timescaledb_endpoint" {
  description = "TimescaleDB endpoint"
  value       = module.timescaledb.db_instance_endpoint
}

output "redis_endpoint" {
  description = "Redis endpoint"
  value       = aws_elasticache_cluster.redis.cache_nodes[0].address
}

output "models_bucket" {
  description = "S3 bucket for model storage"
  value       = aws_s3_bucket.models.id
}
