variable "region" {
  description = "AWS region."
  type        = string
  default     = "ap-south-1"
}

variable "instance_type" {
  description = "EC2 type. t3.micro (2 vCPU, 1 GB) is enough for the demo."
  type        = string
  default     = "t3.micro"
}

variable "ssh_key_name" {
  description = "Name of an existing EC2 key pair to log in with."
  type        = string
}

variable "ssh_cidr" {
  description = "Your IP in CIDR form (e.g. 203.0.113.7/32). Only this may SSH in."
  type        = string
  validation {
    condition     = can(cidrhost(var.ssh_cidr, 0)) && var.ssh_cidr != "0.0.0.0/0"
    error_message = "Use your own IP as a /32, not 0.0.0.0/0."
  }
}

variable "repo_url" {
  description = "Public Git URL of this project. Empty = only install Docker; copy the code yourself."
  type        = string
  default     = ""
}

variable "domain" {
  description = "Your own hostname (point an A record at the output IP). Empty = use <ip>.sslip.io."
  type        = string
  default     = ""
}

variable "max_users" {
  description = "Most virtual users a visitor may start."
  type        = number
  default     = 150
}

variable "budget_email" {
  description = "Email for a monthly cost alert. Empty = no budget is created."
  type        = string
  default     = ""
}

variable "monthly_budget_usd" {
  description = "Alert when forecast monthly cost passes this."
  type        = number
  default     = 15
}
