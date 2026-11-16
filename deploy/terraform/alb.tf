resource "aws_lb" "main" {
  name               = var.name
  load_balancer_type = "application"
  subnets            = aws_subnet.public[*].id
  security_groups    = [aws_security_group.alb.id]
}

# Two target groups: the canary gateway alone, and the rest. The listener
# splits traffic by weight, so a canary rollout exposes canary_weight% of
# traffic instead of 1/N (the E11 laptop runs used equal round robin).
resource "aws_lb_target_group" "canary" {
  name        = "${var.name}-canary"
  port        = 8080
  protocol    = "HTTP"
  target_type = "ip"
  vpc_id      = aws_vpc.main.id
  health_check {
    path     = "/readyz"
    port     = "9090"
    interval = 5
  }
}

resource "aws_lb_target_group" "main" {
  name        = "${var.name}-main"
  port        = 8080
  protocol    = "HTTP"
  target_type = "ip"
  vpc_id      = aws_vpc.main.id
  health_check {
    path     = "/readyz"
    port     = "9090"
    interval = 5
  }
}

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.main.arn
  port              = 80
  protocol          = "HTTP"
  default_action {
    type = "forward"
    forward {
      target_group {
        arn    = aws_lb_target_group.canary.arn
        weight = var.canary_weight
      }
      target_group {
        arn    = aws_lb_target_group.main.arn
        weight = 100 - var.canary_weight
      }
    }
  }
}
