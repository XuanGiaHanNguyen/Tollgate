# Cost switch: with enable_aws=false (the default) the plan fails here, so
# `terraform apply` creates nothing. Preconditions are only checked on create
# and update, so `terraform destroy` still works without the flag.
resource "terraform_data" "cost_guard" {
  lifecycle {
    precondition {
      condition     = var.enable_aws
      error_message = "AWS is switched off (enable_aws=false), so nothing will be created. Pass -var enable_aws=true to deploy and pay for it."
    }
  }
}
