variable "hcloud_token" {
  type      = string
  sensitive = true
}

variable "run_id" {
  type = string
}

variable "ssh_public_key_path" {
  type = string
}

variable "location" {
  type    = string
  default = "nbg1"
}

variable "network_zone" {
  type    = string
  default = "eu-central"
}

variable "image" {
  type    = string
  default = "ubuntu-24.04"
}

variable "controlplane_server_type" {
  type    = string
  default = ""
}

variable "agent_server_type" {
  type    = string
  default = ""
}

variable "network_cidr" {
  type    = string
  default = "10.180.0.0/16"
}

variable "subnet_cidr" {
  type    = string
  default = "10.180.1.0/24"
}

variable "controlplane_private_ip" {
  type    = string
  default = "10.180.1.10"
}

variable "agent_private_ips" {
  type = map(string)
  default = {
    "agent-a" = "10.180.1.21"
    "agent-b" = "10.180.1.22"
  }
}
