output "alb_url" {
  description = "Public entry point (gateways behind the ALB)."
  value       = "http://${aws_lb.main.dns_name}"
}

output "ecr_repositories" {
  value = { for b, r in aws_ecr_repository.bin : b => r.repository_url }
}

output "cluster" {
  value = aws_ecs_cluster.main.name
}
