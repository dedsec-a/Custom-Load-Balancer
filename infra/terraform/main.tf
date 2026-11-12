# One EC2 instance running the Docker Compose stack, behind a fixed Elastic IP.

data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"] # Canonical
  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }
  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

resource "aws_security_group" "web" {
  name        = "loadbalancer-demo"
  description = "Web from anywhere, SSH from one address"

  ingress {
    description = "HTTP (Caddy redirects to HTTPS; needed for the certificate check)"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  ingress {
    description = "HTTPS"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  ingress {
    description = "SSH from the operator only"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [var.ssh_cidr]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

# Allocated first so the boot script can build the sslip.io hostname from it.
resource "aws_eip" "this" {
  domain = "vpc"
}

locals {
  site_address = var.domain != "" ? var.domain : "${replace(aws_eip.this.public_ip, ".", "-")}.sslip.io"
}

resource "aws_instance" "app" {
  ami                    = data.aws_ami.ubuntu.id
  instance_type          = var.instance_type
  key_name               = var.ssh_key_name
  vpc_security_group_ids = [aws_security_group.web.id]

  # Throttle instead of billing extra when the CPU allowance runs out.
  credit_specification {
    cpu_credits = "standard"
  }

  root_block_device {
    volume_type = "gp3"
    volume_size = 16
    encrypted   = true
  }

  metadata_options {
    http_tokens = "required" # IMDSv2 only
  }

  user_data = templatefile("${path.module}/user_data.sh.tftpl", {
    repo_url     = var.repo_url
    site_address = local.site_address
    max_users    = var.max_users
  })
  user_data_replace_on_change = true

  tags = { Name = "loadbalancer-demo" }
}

resource "aws_eip_association" "this" {
  instance_id   = aws_instance.app.id
  allocation_id = aws_eip.this.id
}

resource "aws_budgets_budget" "monthly" {
  count        = var.budget_email == "" ? 0 : 1
  name         = "loadbalancer-demo-monthly"
  budget_type  = "COST"
  limit_amount = tostring(var.monthly_budget_usd)
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  cost_types {
    include_credit = false # alert on usage before credits, so they aren't drained silently
  }

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 80
    threshold_type             = "PERCENTAGE"
    notification_type          = "FORECASTED"
    subscriber_email_addresses = [var.budget_email]
  }
}
