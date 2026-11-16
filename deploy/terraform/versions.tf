terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.80"
    }
  }
}

# Credentials come from the environment (AWS_PROFILE or AWS_ACCESS_KEY_ID /
# AWS_SECRET_ACCESS_KEY); nothing secret lives in this repo.
provider "aws" {
  region = var.region
  default_tags {
    tags = { project = "tollgate", managed_by = "terraform" }
  }
}
