# Runs offline with a mock provider: no AWS credentials, nothing created.
mock_provider "aws" {
  mock_data "aws_availability_zones" {
    defaults = { names = ["us-east-1a", "us-east-1b"] }
  }
  mock_data "aws_iam_policy_document" {
    defaults = { json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}" }
  }
}

variables {
  allowed_cidr = "203.0.113.1/32"
  budget_email = "alerts@example.com"
}

run "off_by_default_blocks_plan" {
  command         = plan
  expect_failures = [terraform_data.cost_guard]
}

run "on_plans_the_stack" {
  command = plan
  variables {
    enable_aws = true
  }
}
