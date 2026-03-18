provider "hcloud" {
  token = var.hcloud_token
}

locals {
  labels = {
    "ebpf-wg-mesh.run-id" = var.run_id
    "ebpf-wg-mesh.scope"  = "test-vm"
  }
}

resource "hcloud_ssh_key" "runner" {
  name       = "${var.run_id}-runner"
  public_key = file(var.ssh_public_key_path)
  labels     = local.labels
}

resource "hcloud_network" "test" {
  name     = "${var.run_id}-network"
  ip_range = var.network_cidr
  labels   = local.labels
}

resource "hcloud_network_subnet" "test" {
  network_id   = hcloud_network.test.id
  type         = "cloud"
  network_zone = var.network_zone
  ip_range     = var.subnet_cidr
}

resource "hcloud_server" "controlplane" {
  name        = "${var.run_id}-controlplane"
  server_type = var.controlplane_server_type
  image       = var.image
  location    = var.location
  ssh_keys    = [hcloud_ssh_key.runner.id]
  labels      = merge(local.labels, { "ebpf-wg-mesh.role" = "controlplane" })

  public_net {
    ipv4_enabled = true
    ipv6_enabled = true
  }

  network {
    network_id = hcloud_network.test.id
    ip         = var.controlplane_private_ip
  }

  user_data = templatefile("${path.module}/../cloud-init/controlplane.yaml.tftpl", {
    hostname = "${var.run_id}-controlplane"
  })

  depends_on = [hcloud_network_subnet.test]
}

resource "hcloud_server" "agents" {
  for_each    = var.agent_private_ips
  name        = "${var.run_id}-${each.key}"
  server_type = var.agent_server_type
  image       = var.image
  location    = var.location
  ssh_keys    = [hcloud_ssh_key.runner.id]
  labels      = merge(local.labels, { "ebpf-wg-mesh.role" = "agent", "ebpf-wg-mesh.agent-name" = each.key })

  public_net {
    ipv4_enabled = true
    ipv6_enabled = true
  }

  network {
    network_id = hcloud_network.test.id
    ip         = each.value
  }

  user_data = templatefile("${path.module}/../cloud-init/agent.yaml.tftpl", {
    hostname = "${var.run_id}-${each.key}"
  })

  depends_on = [hcloud_network_subnet.test]
}
