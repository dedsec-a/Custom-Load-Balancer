output "public_ip" {
  value = aws_eip.this.public_ip
}

output "url" {
  description = "Live after the first boot finishes (about 5-10 minutes when building from the repo)."
  value       = "https://${local.site_address}"
}

output "ssh" {
  value = "ssh -i <your-key>.pem ubuntu@${aws_eip.this.public_ip}"
}
