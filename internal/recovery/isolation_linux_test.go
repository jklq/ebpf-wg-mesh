//go:build linux

package recovery

import (
	"net"
	"os"
	"os/exec"
	"testing"
)

func TestIsolationRefusesCallerNamespace(t *testing.T) {
	parent, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareIsolation(parent); err == nil {
		t.Fatal("caller network namespace accepted")
	}
}

func TestFreshNetworkNamespace(t *testing.T) {
	if parent := os.Getenv("RECOVERY_TEST_PARENT_NS"); parent != "" {
		if err := prepareIsolation(parent); err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal("isolated loopback unavailable", err)
		}
		listener.Close()
		connection, err := net.Dial("tcp", "192.0.2.1:443")
		if err == nil {
			connection.Close()
			t.Fatal("isolated namespace reached external network")
		}
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("requires namespace privileges; run the compiled test with sudo")
	}
	parent, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/unshare", "--net", "--pid", "--fork", "--mount-proc", "--kill-child", binary, "-test.run=^TestFreshNetworkNamespace$")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "RECOVERY_TEST_PARENT_NS=" + parent}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("namespace restore runner: %s: %v", b, err)
	}
}
