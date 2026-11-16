variable "region" {
  description = "AWS region."
  type        = string
  default     = "us-east-1"
}

variable "name" {
  description = "Prefix for every resource."
  type        = string
  default     = "tollgate"
}

variable "image_tag" {
  description = "Tag of the images pushed to ECR (see README)."
  type        = string
  default     = "latest"
}

variable "allowed_cidr" {
  description = "CIDR allowed to reach the ALB (set this to your IP/32)."
  type        = string
}

variable "budget_limit_usd" {
  description = "Monthly AWS budget; alerts at 50%, 80% and forecasted 100%."
  type        = number
  default     = 10
}

variable "budget_email" {
  description = "Email for budget alerts."
  type        = string
}

variable "canary_weight" {
  description = "ALB weight (0-100) of the canary gateway's target group; the other gateways share 100 - canary_weight. Equal-weight round robin gives the canary 1/N of traffic; a low weight keeps rollout exposure under 1/N."
  type        = number
  default     = 5
}

variable "gateway_cpu" {
  description = "Fargate CPU units per gateway task (1024 = 1 vCPU)."
  type        = number
  default     = 1024
}

variable "gateway_memory" {
  description = "Fargate memory (MiB) per gateway task."
  type        = number
  default     = 2048
}
