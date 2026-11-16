locals {
  binaries = ["tollgate-gw", "tollgate-control", "tollgatectl", "mockbackend", "mockllm"]
}

resource "aws_ecr_repository" "bin" {
  for_each             = toset(local.binaries)
  name                 = "${var.name}/${each.key}"
  image_tag_mutability = "MUTABLE"
  force_delete         = true # `terraform destroy` must leave nothing behind
}

locals {
  image = { for b in local.binaries : b => "${aws_ecr_repository.bin[b].repository_url}:${var.image_tag}" }
}
