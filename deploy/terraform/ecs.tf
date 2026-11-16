resource "aws_ecs_cluster" "main" {
  name = var.name
}

resource "aws_cloudwatch_log_group" "main" {
  name              = "/ecs/${var.name}"
  retention_in_days = 3
}

data "aws_iam_policy_document" "ecs_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "exec" {
  name               = "${var.name}-task-exec"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
}

resource "aws_iam_role_policy_attachment" "exec" {
  role       = aws_iam_role.exec.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

# ---- service discovery: every task gets a stable name in tollgate.local ----

resource "aws_service_discovery_private_dns_namespace" "main" {
  name = "${var.name}.local"
  vpc  = aws_vpc.main.id
}

locals {
  domain = "${var.name}.local"
  ctl    = ["ctl-1", "ctl-2", "ctl-3"]
  gws    = ["gw-1", "gw-2", "gw-3"]
  backs  = ["backend-1", "backend-2", "backend-3"]
  llms   = ["provider-a", "provider-b"]

  raft_peers = join(",", [for i, n in local.ctl : "${i + 1}=${n}.${local.domain}:7100"])
  api_peers  = join(",", [for i, n in local.ctl : "${i + 1}=${n}.${local.domain}:7200"])
  gw_admins  = join(",", [for n in local.gws : "${n}=http://${n}.${local.domain}:9090"])
}

resource "aws_service_discovery_service" "svc" {
  for_each = toset(concat(local.ctl, local.gws, local.backs, local.llms))
  name     = each.key
  dns_config {
    namespace_id = aws_service_discovery_private_dns_namespace.main.id
    dns_records {
      ttl  = 5
      type = "A"
    }
    routing_policy = "MULTIVALUE"
  }
  health_check_custom_config {
    failure_threshold = 1
  }
}

# ---- Raft storage: EFS, one access point (directory) per control node ----

resource "aws_efs_file_system" "raft" {
  encrypted = true
  tags      = { Name = "${var.name}-raft" }
}

resource "aws_efs_mount_target" "raft" {
  count           = 2
  file_system_id  = aws_efs_file_system.raft.id
  subnet_id       = aws_subnet.private[count.index].id
  security_groups = [aws_security_group.tasks.id]
}

resource "aws_efs_access_point" "ctl" {
  for_each       = toset(local.ctl)
  file_system_id = aws_efs_file_system.raft.id
  posix_user {
    uid = 65532 # distroless nonroot
    gid = 65532
  }
  root_directory {
    path = "/${each.key}"
    creation_info {
      owner_uid   = 65532
      owner_gid   = 65532
      permissions = "0750"
    }
  }
}

locals {
  log = {
    logDriver = "awslogs"
    options = {
      awslogs-group         = aws_cloudwatch_log_group.main.name
      awslogs-region        = var.region
      awslogs-stream-prefix = "tg"
    }
  }
}

# ---- control plane: 3 single-task services ----

resource "aws_ecs_task_definition" "ctl" {
  for_each                 = toset(local.ctl)
  family                   = "${var.name}-${each.key}"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 256
  memory                   = 512
  execution_role_arn       = aws_iam_role.exec.arn
  volume {
    name = "raft"
    efs_volume_configuration {
      file_system_id     = aws_efs_file_system.raft.id
      transit_encryption = "ENABLED"
      authorization_config {
        access_point_id = aws_efs_access_point.ctl[each.key].id
      }
    }
  }
  container_definitions = jsonencode([{
    name        = each.key
    image       = local.image["tollgate-control"]
    essential   = true
    mountPoints = [{ sourceVolume = "raft", containerPath = "/data" }]
    command = ["-id", tostring(index(local.ctl, each.key) + 1), "-data", "/data",
      "-raft-listen", ":7100", "-api-listen", ":7200", "-admin-listen", ":7300",
    "-raft-peers", local.raft_peers, "-api-peers", local.api_peers, "-gateways", local.gw_admins]
    logConfiguration = local.log
  }])
}

resource "aws_ecs_service" "ctl" {
  for_each        = toset(local.ctl)
  name            = each.key
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.ctl[each.key].arn
  desired_count   = 1
  launch_type     = "FARGATE"
  # A Raft member must never run twice: stop the old task before the new one.
  deployment_minimum_healthy_percent = 0
  deployment_maximum_percent         = 100
  network_configuration {
    subnets         = aws_subnet.private[*].id
    security_groups = [aws_security_group.tasks.id]
  }
  service_registries {
    registry_arn = aws_service_discovery_service.svc[each.key].arn
  }
  depends_on = [aws_efs_mount_target.raft]
}

# ---- mock backends and mock LLM providers ----

resource "aws_ecs_task_definition" "mock" {
  for_each                 = toset(concat(local.backs, local.llms))
  family                   = "${var.name}-${each.key}"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 256
  memory                   = 512
  execution_role_arn       = aws_iam_role.exec.arn
  container_definitions = jsonencode([{
    name      = each.key
    image     = contains(local.llms, each.key) ? local.image["mockllm"] : local.image["mockbackend"]
    essential = true
    command = contains(local.llms, each.key) ? [
      "-addr", ":9200", "-name", each.key, "-ttft-median", "100ms", "-tail-prob", "0.05", "-tail-mult", "10"
    ] : ["-addr", ":9000", "-name", each.key]
    logConfiguration = local.log
  }])
}

resource "aws_ecs_service" "mock" {
  for_each        = toset(concat(local.backs, local.llms))
  name            = each.key
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.mock[each.key].arn
  desired_count   = 1
  launch_type     = "FARGATE"
  network_configuration {
    subnets         = aws_subnet.private[*].id
    security_groups = [aws_security_group.tasks.id]
  }
  service_registries {
    registry_arn = aws_service_discovery_service.svc[each.key].arn
  }
}

# ---- gateways: one service each (stable names for the hash ring and canary) ----

resource "aws_ecs_task_definition" "gw" {
  for_each                 = toset(local.gws)
  family                   = "${var.name}-${each.key}"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.gateway_cpu
  memory                   = var.gateway_memory
  execution_role_arn       = aws_iam_role.exec.arn
  container_definitions = jsonencode([{
    name         = each.key
    image        = local.image["tollgate-gw"]
    essential    = true
    command      = ["-config-env", "TOLLGATE_CONFIG", "-node", each.key, "-cluster-self", each.key]
    portMappings = [{ containerPort = 8080 }, { containerPort = 9090 }]
    environment = [{
      name  = "TOLLGATE_CONFIG"
      value = templatefile("${path.module}/gateway.yaml.tftpl", { domain = local.domain, gws = local.gws, ctl = local.ctl, backs = local.backs })
    }]
    logConfiguration = local.log
  }])
}

resource "aws_ecs_service" "gw" {
  for_each        = toset(local.gws)
  name            = each.key
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.gw[each.key].arn
  desired_count   = 1
  launch_type     = "FARGATE"
  network_configuration {
    subnets         = aws_subnet.private[*].id
    security_groups = [aws_security_group.tasks.id]
  }
  service_registries {
    registry_arn = aws_service_discovery_service.svc[each.key].arn
  }
  load_balancer {
    target_group_arn = each.key == local.gws[0] ? aws_lb_target_group.canary.arn : aws_lb_target_group.main.arn
    container_name   = each.key
    container_port   = 8080
  }
  depends_on = [aws_lb_listener.http]
}

# ---- k6 load generator (run on demand in the same region) ----

resource "aws_ecs_task_definition" "k6" {
  family                   = "${var.name}-k6"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 2048
  memory                   = 4096
  execution_role_arn       = aws_iam_role.exec.arn
  container_definitions = jsonencode([{
    name             = "k6"
    image            = "grafana/k6:latest"
    essential        = true
    command          = ["run", "/dev/stdin"] # overridden per run (see README)
    logConfiguration = local.log
  }])
}
