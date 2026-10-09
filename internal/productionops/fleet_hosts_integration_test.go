//go:build integration && linux

package productionops

import (
	"ebof-wg-mesh/internal/deploy"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func (f *nativeFleet) prepareHost(h deploy.Host) deploy.Host {
	f.t.Helper()
	t := f.t
	var err error
	name := f.id + "-" + h.ID
	f.mu.Lock()
	f.hosts[h.ID] = name
	f.mu.Unlock()
	hostRoot := f.ram + "/" + h.ID
	f.command("mkdir", "-p", hostRoot+"/opt", hostRoot+"/var")
	f.docker("run", "-d", "--name", name, "--network", f.network, "--label", "platform.native-fixture="+f.id, "--privileged", "--cgroupns=private", "--tmpfs", "/run", "--tmpfs", "/run/lock", "--tmpfs", "/tmp", "--tmpfs", "/var/lib/containerd", "--mount", "type=bind,src="+hostRoot+"/opt,dst=/opt/ebpf-wg-mesh", "--mount", "type=bind,src="+hostRoot+"/var,dst=/var/lib/ebpf-wg-mesh", f.image)
	var found []struct {
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress, GlobalIPv6Address string }
		}
	}
	if err = json.Unmarshal(f.docker("inspect", name), &found); err != nil {
		t.Fatal(err)
	}
	address := found[0].NetworkSettings.Networks[f.network]
	h.Binding = deploy.Binding{Provider: "imported", ServerID: name}
	h.SSH = deploy.SSH{Address: address.IPAddress, User: "root", Key: "ssh", KnownHosts: f.root + "/known_hosts"}
	h.Network.Address = address.GlobalIPv6Address
	h.FailureDomain = "fixture/" + h.ID
	h.Capabilities = []string{"systemd", "containerd", "wireguard", "ebpf-policy", "cgroup-v2"}
	script := "set -eu\nrm -f /etc/ssh/ssh_host_*\nssh-keygen -A >/dev/null\ninstall -d -m 0700 /root/.ssh\n"
	script += remoteFile("/root/.ssh/authorized_keys", f.public) + remoteFile("/usr/local/share/ca-certificates/fixture.crt", f.allCA) + "update-ca-certificates >/dev/null\nmountpoint -q /sys/fs/bpf || mount -t bpf bpf /sys/fs/bpf\nmkdir -p /run/sshd\nsystemctl reset-failed ssh\nsystemctl restart ssh\n"
	script += remoteFile("/etc/cni/net.d/10-mesh-cni.conflist", []byte(`{"cniVersion":"1.0.0","name":"mesh-cni","plugins":[{"type":"bridge","bridge":"mesh0","isGateway":true,"ipMasq":false,"capabilities":{"ips":true},"ipam":{"type":"host-local","ranges":[[{"subnet":"10.200.0.0/16"}],[{"subnet":"fd00:200::/48"}]],"routes":[{"dst":"0.0.0.0/0"},{"dst":"::/0"}]}},{"type":"loopback"}]}`))
	script += remoteFile("/etc/cni/build-sandbox.d/10-build.conflist", []byte(`{"cniVersion":"1.0.0","name":"build-sandbox","plugins":[{"type":"bridge","bridge":"build0","isGateway":true,"ipMasq":true,"ipam":{"type":"host-local","subnet":"10.201.0.0/16"}},{"type":"loopback"}]}`))
	script += "sysctl -qw net.ipv4.ip_forward=1\nsysctl -qw net.ipv6.conf.all.forwarding=1\n"
	f.docker("exec", name, "sh", "-c", script)
	key := strings.Fields(string(f.docker("exec", name, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")))
	file, err := os.OpenFile(f.root+"/known_hosts", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(file, "%s %s %s\n", address.IPAddress, key[0], key[1])
	file.Close()
	return h
}
