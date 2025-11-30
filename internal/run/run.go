package run

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	stdexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/creotiv/toy-docker/internal/exec"
)

const (
	imagesDir  = "images"
	bridgeName = "toy0"
	bridgeCIDR = "10.200.0.1/24"
	netRange   = "10.200.0.0/24"
)

type Meta struct {
	Name   string `json:"name"`
	Parent string `json:"parent"`
}

func ensureBridge() error {
	// check if exists
	out, err := exec.RunOut("ip", "link", "show", bridgeName)
	if err == nil && strings.Contains(out, bridgeName) {
		fmt.Println("[net] bridge exists: ", bridgeName)
		return nil
	}

	fmt.Println("[net] creating bridge", bridgeName)
	if err := exec.RunOrErr("add bridge", "ip", "link", "add", bridgeName, "type", "bridge"); err != nil {
		return err
	}
	if err := exec.RunOrErr("assign bridge ip", "ip", "addr", "add", bridgeCIDR, "dev", bridgeName); err != nil {
		return err
	}
	if err := exec.RunOrErr("bring bridge up", "ip", "link", "set", bridgeName, "up"); err != nil {
		return err
	}

	// make sure forwarding is allowed so NAT works
	if err := exec.RunOrErr("[net] enable ipv4 forwarding", "sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return err
	}

	if err := exec.RunOrErr("[net] configure nat", "iptables", "-t", "nat", "-A", "POSTROUTING",
		"-s", netRange, "!", "-o", bridgeName, "-j", "MASQUERADE"); err != nil {
		return err
	}
	// allow traffic to flow through the bridge
	if err := exec.RunOrErr("[net] allow forward from bridge", "iptables", "-A", "FORWARD", "-i", bridgeName, "-j", "ACCEPT"); err != nil {
		return err
	}
	if err := exec.RunOrErr("[net] allow established to bridge", "iptables", "-A", "FORWARD", "-o", bridgeName, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"); err != nil {
		return err
	}

	return nil
}

func RunContainer(image string, cmd []string, volumes string, ports string) error {
	if err := ensureBridge(); err != nil {
		return err
	}

	contDir := containersDir()
	if err := os.MkdirAll(contDir, 0755); err != nil {
		return fmt.Errorf("prepare containers dir: %w", err)
	}

	imgDir := imagesDir + "/" + image
	layer := imgDir + "/layer.tar"

	cid := fmt.Sprintf("%d-%d", os.Getpid(), os.Getpid()+12345)
	rootfs := filepath.Join(contDir, cid, "rootfs")
	if err := os.MkdirAll(rootfs, 0755); err != nil {
		return fmt.Errorf("create rootfs: %w", err)
	}

	// extract
	if err := exec.RunOrErr("[fs] extract base layer", "tar", "-C", rootfs, "-xf", layer); err != nil {
		return err
	}
	if err := writeResolvConf(rootfs); err != nil {
		return err
	}

	// Allocate IP
	ip := fmt.Sprintf("10.200.0.%d", 10+os.Getpid()%200)

	vethH := "vethh" + cid[:6]
	vethC := "vethc" + cid[:6]

	// Create veth pair
	if err := exec.RunOrErr("[net] create veth", "ip", "link", "add", vethH, "type", "veth", "peer", "name", vethC); err != nil {
		return err
	}

	// Put host side into bridge
	if err := exec.RunOrErr("[net] attach veth to bridge", "ip", "link", "set", vethH, "master", bridgeName); err != nil {
		return err
	}
	if err := exec.RunOrErr("[net] bring veth up", "ip", "link", "set", vethH, "up"); err != nil {
		return err
	}

	// Prepare netns file (Linux trick)
	nsfile := "/var/run/toy-" + cid + ".ns"
	if err := os.WriteFile(nsfile, []byte{}, 0644); err != nil {
		return fmt.Errorf("create ns file: %w", err)
	}

	// Prepare container-init MustRun
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self executable: %w", err)
	}
	allCmd := []string{
		"unshare",
		"--fork",
		"--pid",
		"--net",
		"--ipc",
		"--uts",
		"--mount",
		"--mount-proc",
		self, "init",
	}

	// Start container
	c := stdexec.Command(allCmd[0], allCmd[1:]...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Stdin = os.Stdin
	c.Env = append(os.Environ(),
		"ROOTFS="+rootfs,
		"CID="+cid,
		"CIP="+ip,
		"VETH="+vethC,
		"VOLUMES="+volumes,
		"CMD="+strings.Join(cmd, " "),
	)

	fmt.Println("[run] starting container namespace")

	if err := c.Start(); err != nil {
		return fmt.Errorf("container init failed: %w", err)
	}

	// Wait for the child to finish unsharing its network namespace before moving the veth
	// inside. Without this, we sometimes race and move the veth into the host netns because
	// the child hasn't called unshare yet, so the interface never shows up inside the container.
	if err := waitForChildNetns(c.Process.Pid); err != nil {
		return err
	}

	// Move vethC into container netns using the child pid
	pid := fmt.Sprintf("%d", c.Process.Pid)
	if err := exec.RunOrErr("[net] move veth to container", "ip", "link", "set", vethC, "netns", pid); err != nil {
		return err
	}

	// Port forwarding
	if err := configurePorts(ip, ports); err != nil {
		return err
	}

	return c.Wait()
}

func containersDir() string {
	if v := os.Getenv("TOY_DOCKER_CONTAINERS"); v != "" {
		return v
	}
	// Default to tmpfs/host-local disk to avoid permission quirks on shared mounts
	// (e.g., macOS host paths inside Lima with root-squash semantics).
	return filepath.Join(os.TempDir(), "toy-docker", "containers")
}

func configurePorts(ip, ports string) error {
	if ports == "" {
		return nil
	}
	for _, p := range strings.Split(ports, ";") {
		if p == "" {
			continue
		}
		parts := strings.Split(p, ":")
		if len(parts) != 2 {
			return fmt.Errorf("invalid port mapping: %s", p)
		}
		hp, cp := parts[0], parts[1]
		if err := exec.RunOrErr("[net] add port forward", "iptables", "-t", "nat", "-A", "PREROUTING",
			"-p", "tcp", "--dport", hp,
			"-j", "DNAT", "--to-destination", fmt.Sprintf("%s:%s", ip, cp)); err != nil {
			return err
		}
	}
	return nil
}

// waitForChildNetns blocks until the child process has switched to a new netns.
// This avoids racing the "ip link set ... netns" call before unshare(2) runs,
// which would leave the veth stuck in the host namespace.
func waitForChildNetns(pid int) error {
	hostNetns, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return fmt.Errorf("read host netns: %w", err)
	}

	target := fmt.Sprintf("/proc/%d/ns/net", pid)
	for i := 0; i < 50; i++ {
		childNetns, err := os.Readlink(target)
		if err == nil {
			if childNetns != hostNetns {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for child netns for pid %d", pid)
}

// writeResolvConf injects a usable DNS configuration into the container rootfs.
// It prefers the host resolv.conf but strips loopback stubs; falls back to public resolvers.
func writeResolvConf(rootfs string) error {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return fmt.Errorf("read host resolv.conf: %w", err)
	}
	if bytes.Contains(data, []byte("127.0.0.53")) {
		if real, err := os.ReadFile("/run/systemd/resolve/resolv.conf"); err == nil && len(real) > 0 {
			data = real
		}
	}

	var filtered bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(data))
	added := 0
	seen := map[string]bool{}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "nameserver") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			ns := fields[1]
			if strings.HasPrefix(ns, "127.") || ns == "::1" {
				continue
			}
			if !seen[ns] {
				fmt.Fprintf(&filtered, "nameserver %s\n", ns)
				seen[ns] = true
				added++
			}
			continue
		}
		filtered.WriteString(sc.Text())
		filtered.WriteByte('\n')
	}
	// Always provide public resolvers as reliable fallback
	publicResolvers := []string{"1.1.1.1", "8.8.8.8"}
	for _, ns := range publicResolvers {
		if !seen[ns] {
			if added == 0 && filtered.Len() > 0 {
				filtered.Reset()
			}
			fmt.Fprintf(&filtered, "nameserver %s\n", ns)
			seen[ns] = true
			added++
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("scan host resolv.conf: %w", err)
	}

	dst := filepath.Join(rootfs, "etc", "resolv.conf")
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("ensure etc dir: %w", err)
	}
	if err := os.WriteFile(dst, filtered.Bytes(), 0644); err != nil {
		return fmt.Errorf("write container resolv.conf: %w", err)
	}
	return nil
}
