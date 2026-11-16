# AWS deployment (Terraform)

**Status: written and `terraform validate`d, never applied.** No AWS resources have been created from this repo. Applying it costs money; read the cost section first.

## What it builds

- VPC with 2 public subnets (ALB, NAT) and 2 private subnets (all tasks); one NAT gateway.
- ECS Fargate services, each with a stable Cloud Map name in `tollgate.local`:
  - `ctl-1..3`: Raft control plane, each with its own EFS access point for the WAL (a replaced task finds its data again; deployments stop the old task before starting the new one, so a member never runs twice);
  - `gw-1..3`: gateways (cluster rate limiting over gRPC, rules from the control plane, config passed via `-config-env`);
  - `backend-1..3`, `provider-a`, `provider-b`: mocks.
- **ALB** with two weighted target groups: `gw-1` alone (the canary) at `canary_weight`% (default 5) and the others at the rest. A canary rollout (`tollgatectl rules rollout <file> gw-1`) then exposes 5% of traffic instead of 1/3, which is the M7 exposure goal the equal-weight laptop runs could not meet.
- ECR repositories for the five binaries (`force_delete`, so destroy leaves nothing).
- A k6 task definition for load from inside the region.
- A monthly **budget** with email alerts at 50% and 80% actual and 100% forecasted spend.

## Cost estimate (unverified; check the AWS Pricing Calculator before applying)

Approximate us-east-1 on-demand list prices as I last knew them; prices change, so treat these as an order of magnitude:

| Item | Size | ≈ $/hour |
| --- | --- | --- |
| 3 gateways | 1 vCPU, 2 GB each (Fargate ≈ $0.0405/vCPU-h + $0.0044/GB-h) | 0.148 |
| 3 control nodes | 0.25 vCPU, 0.5 GB | 0.037 |
| 5 mocks | 0.25 vCPU, 0.5 GB | 0.062 |
| k6 (only while a test runs) | 2 vCPU, 4 GB | 0.099 |
| ALB | base + low LCUs | ≈ 0.03 |
| NAT gateway | base + image pulls / logs | ≈ 0.05 |
| EFS, CloudWatch Logs (3-day retention), ECR | small | < 0.01 |
| **Total** | | **≈ $0.35–0.45 per hour** |

A 2-hour benchmark session is roughly $1; leaving it up for a month would be roughly $250–330. **Run `terraform destroy` after every session.**

## Run (if you decide to)

```sh
export AWS_PROFILE=...          # credentials stay in your environment
cd deploy/terraform
terraform init
terraform apply -var allowed_cidr=$(curl -s https://checkip.amazonaws.com)/32 -var budget_email=you@example.com

# push images (from the repo root)
REG=$(terraform -chdir=deploy/terraform output -json ecr_repositories | jq -r '."tollgate-gw"' | cut -d/ -f1)
aws ecr get-login-password | docker login --username AWS --password-stdin $REG
for b in tollgate-gw tollgate-control tollgatectl mockbackend mockllm; do
  docker build --build-arg BIN=$b -t $REG/tollgate/$b:latest . && docker push $REG/tollgate/$b:latest
done
aws ecs update-service --cluster tollgate --service gw-1 --force-new-deployment   # etc., or re-apply

# push rules through the control plane (from a task in the VPC, e.g. ECS Exec into gw-1), then benchmark
# from the k6 task with the ALB URL (terraform output alb_url); record AWS task sizes in each result file.

terraform destroy -var allowed_cidr=0.0.0.0/32 -var budget_email=you@example.com
```

## Not verified

`terraform validate` checks syntax and provider schemas only. Whether the stack comes up and passes the M8 acceptance ("`terraform apply` brings up the full stack from scratch; `terraform destroy` leaves nothing running") has **not** been tested, because nothing was applied.
