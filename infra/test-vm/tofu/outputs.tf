output "hosts" {
  value = merge(
    {
      controlplane = {
        role         = "controlplane"
        name         = hcloud_server.controlplane.name
        public_ipv4  = hcloud_server.controlplane.ipv4_address
        public_ipv6  = hcloud_server.controlplane.ipv6_address
        private_ipv4 = one(hcloud_server.controlplane.network).ip
      }
    },
    {
      for name, server in hcloud_server.agents :
      name => {
        role         = "agent"
        name         = server.name
        public_ipv4  = server.ipv4_address
        public_ipv6  = server.ipv6_address
        private_ipv4 = one(server.network).ip
      }
    },
  )
}
