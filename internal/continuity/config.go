// Package continuity owns stable TCP listeners and bounded process overlap.
// It never retries application requests or terminates a connection to complete
// an upgrade. An unfinished drain is a pending upgrade, not permission to kill.
package continuity

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const Protocol = 1
const ListenerPortsMetadata = "platform_listener_ports"

type Listener struct {
	Address  string `json:"address" yaml:"address"`
	Env      string `json:"env" yaml:"env"`
	PortOnly bool   `json:"portOnly,omitempty" yaml:"portOnly,omitempty"`
}

// Launch contains resolved, private inputs. ID fingerprints their contents,
// rather than filenames. Every launch has its own immutable configuration.
type Launch struct {
	ID          string            `json:"id"`
	Command     []string          `json:"command"`
	Environment map[string]string `json:"environment"`
	Directory   string            `json:"directory"`
	ConfigDir   string            `json:"configDir"`
	Listeners   []Listener        `json:"listeners"`
	Ready       []string          `json:"ready"`
	// Stop prepares a superseded generation for SIGTERM, as systemd ExecStop=
	// does for the owner: it withdraws and drains with the generation's own
	// private addresses in its environment.
	Stop             []string `json:"stop,omitempty"`
	MemoryMiB        int64    `json:"memoryMiB"`
	CPUMillis        int64    `json:"cpuMillis"`
	EnforceResources bool     `json:"enforceResources"`
	EnvFiles         []string `json:"envFiles,omitempty"`
	ConfigDigest     string   `json:"configDigest"`
	ConfigOrigin     string   `json:"configOrigin,omitempty"`
}

func Prepare(path string) (Launch, error) {
	var l Launch
	b, err := os.ReadFile(path)
	if err != nil {
		return l, err
	}
	if err = json.Unmarshal(b, &l); err != nil {
		return l, err
	}
	l.Environment, err = ReadEnvironment(l.EnvFiles...)
	if err != nil {
		return l, err
	}
	l.ConfigDigest, err = directoryDigest(l.ConfigDir)
	if err != nil {
		return l, err
	}
	l.ID = ""
	l.ID = Digest(l)
	return l, l.Validate()
}

func included(name string) bool {
	return name != "launch.json" && name != "launch.env" && name != "staged" && name != "configured" && name != "inspection-probe.json" && !strings.HasSuffix(name, ".next")
}
func directoryDigest(root string) (string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !included(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			files[rel] = "link:" + link
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = Digest(b)
		return nil
	})
	return Digest(files), err
}

func (l Launch) Validate() error {
	if len(l.ID) != 64 || len(l.Command) == 0 || !filepath.IsAbs(l.Command[0]) || len(l.Ready) == 0 || !filepath.IsAbs(l.ConfigDir) || !filepath.IsAbs(l.Directory) || l.MemoryMiB <= 0 || len(l.Listeners) == 0 {
		return fmt.Errorf("incomplete continuity launch")
	}
	if _, err := hex.DecodeString(l.ID); err != nil {
		return err
	}
	if l.EnforceResources && l.CPUMillis <= 0 {
		return fmt.Errorf("native resource enforcement requires a positive CPU budget")
	}
	seen := map[string]bool{}
	for _, p := range l.Listeners {
		if p.Env == "" || strings.ContainsAny(p.Env, "=\n\r") || seen[p.Address] {
			return fmt.Errorf("invalid or duplicate continuity listener")
		}
		seen[p.Address] = true
	}
	return nil
}

func ReadLaunch(path string) (Launch, error) {
	var l Launch
	b, err := os.ReadFile(path)
	if err != nil {
		return l, err
	}
	err = json.Unmarshal(b, &l)
	if err != nil {
		return l, err
	}
	return l, l.Validate()
}

// ReadEnvironment reads the deliberately small quoted format emitted by the
// installer. It does not evaluate shell substitutions or source a shell file.
func ReadEnvironment(paths ...string) (map[string]string, error) {
	env := map[string]string{}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		s := bufio.NewScanner(strings.NewReader(string(b)))
		s.Buffer(make([]byte, 4096), 1<<20)
		for s.Scan() {
			line := strings.TrimSpace(s.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, v, ok := strings.Cut(line, "=")
			if !ok {
				return nil, fmt.Errorf("invalid environment file")
			}
			v, err = strconv.Unquote(v)
			if err != nil {
				return nil, err
			}
			env[key] = v
		}
		if err := s.Err(); err != nil {
			return nil, err
		}
	}
	return env, nil
}

func Digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func save(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".continuity-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
