package run

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/creotiv/toy-docker/internal/exec"
)

func Init() {
	rootfs := os.Getenv("ROOTFS")
	cid := os.Getenv("CID")
	cip := os.Getenv("CIP")
	veth := os.Getenv("VETH")
	vols := os.Getenv("VOLUMES")
	cmd := os.Getenv("CMD")

	fmt.Println("[run] Initing the container")
	// expose netns
	nsfile := "/var/run/toy-" + cid + ".ns"
	// bind-mount current netns so parent can join it later
	exec.MustRun("mount", "--bind", "/proc/self/ns/net", nsfile)

	// stop mount events from propagating out of this namespace
	exec.MustRun("mount", "--make-rprivate", "/")
	// ensure rootfs is a mountpoint we can chroot into
	exec.MustRun("mount", "--bind", rootfs, rootfs)
	// provide device nodes (urandom, null, tty, etc.)
	exec.MustRun("mount", "--rbind", "/dev", rootfs+"/dev")

	// provide /proc inside the container
	exec.MustRun("mount", "-t", "proc", "proc", rootfs+"/proc")

	// volumes
	for _, v := range strings.Split(vols, ";") {
		if v == "" {
			continue
		}
		parts := strings.Split(v, ":")
		src := parts[0]
		dst := rootfs + parts[1]
		os.MkdirAll(dst, 0755)
		// bind-mount host path into container path
		exec.MustRun("mount", "--bind", src, dst)
	}

	// network
	// enable loopback in the container netns
	exec.MustRun("ip", "link", "set", "lo", "up")
	// wait for host to move veth into this netns, then rename to eth0
	if err := waitForVeth(veth); err != nil {
		panic(err)
	}
	fmt.Println("[run] Initing the container 1")
	// assign container IP
	exec.MustRun("ip", "addr", "add", cip+"/24", "dev", "eth0")
	// bring eth0 up
	exec.MustRun("ip", "link", "set", "eth0", "up")
	fmt.Println("[run] Initing the container 2")
	// set default route via host bridge
	exec.MustRun("ip", "route", "add", "default", "via", "10.200.0.1")

	// set container hostname
	exec.MustRun("hostname", "toy-"+cid)

	// chroot + exec MustRun
	// enter rootfs and run the requested command
	exec.MustRun("chroot", rootfs, "/bin/bash", "-c", cmd)

	os.Exit(0)
}

func waitForVeth(name string) error {
	for i := 0; i < 50; i++ {
		if err := exec.Run("ip", "link", "set", name, "name", "eth0"); err == nil {
			return nil
		}
		fmt.Println("[run] Initing the container 2.4")
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("veth %s not present in netns", name)
}
