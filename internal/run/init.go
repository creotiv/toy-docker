package run

import (
	"fmt"
	"os"
	"path/filepath"
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

	fmt.Println("[run] Initing the container --------------------------------")
	fmt.Println("rootfs = ", rootfs)
	fmt.Println("cid = ", cid)
	fmt.Println("cip = ", cip)
	fmt.Println("veth = ", veth)
	fmt.Println("vols = ", vols)
	fmt.Println("cmd = ", cmd)
	fmt.Println("------------------------------------------------------------")

	// expose netns
	nsfile := "/var/run/toy-" + cid + ".ns"
	// bind-mount current netns so parent can join it later
	exec.MustRun("[fs] mount namespace", "mount", "--bind", "/proc/self/ns/net", nsfile)

	// stop mount events from propagating out of this namespace
	exec.MustRun("[fs] mount private", "mount", "--make-rprivate", "/")
	// ensure rootfs is a mountpoint we can chroot into
	exec.MustRun("[fs] mount rootfs(hack)", "mount", "--bind", rootfs, rootfs)
	// provide device nodes (urandom, null, tty, etc.)
	exec.MustRun("[fs] mount dev", "mount", "--rbind", "/dev", rootfs+"/dev")

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
		exec.MustRun("[fs] mount volume", "mount", "--bind", src, dst)
	}

	// network
	// enable loopback in the container netns
	exec.MustRun("[net] set loopback up", "ip", "link", "set", "lo", "up")
	// wait for host to move veth into this netns, then rename to eth0
	if err := waitForVeth(veth); err != nil {
		panic(err)
	}
	// assign container IP
	exec.MustRun("[net] assign IP", "ip", "addr", "add", cip+"/24", "dev", "eth0")
	// bring eth0 up
	exec.MustRun("[net] bring eth0 up", "ip", "link", "set", "eth0", "up")
	// set default route via host bridge
	exec.MustRun("[net] set default route", "ip", "route", "add", "default", "via", "10.200.0.1")

	// set container hostname
	exec.MustRun("[net] set hostname", "hostname", "toy-"+cid)

	// pivot to the new root so the host root disappears from this namespace
	putOld := filepath.Join(rootfs, "old_root")
	if err := os.MkdirAll(putOld, 0700); err != nil {
		panic(fmt.Errorf("create put_old: %w", err))
	}
	if err := exec.RunOrErr("[fs] pivot_root", "pivot_root", rootfs, putOld); err != nil {
		panic(fmt.Errorf("pivot_root: %w", err))
	}
	if err := os.Chdir("/"); err != nil {
		panic(fmt.Errorf("chdir to new root: %w", err))
	}

	// provide /proc inside the container
	exec.MustRun("[fs] mount proc", "mount", "-t", "proc", "blabla", "/proc")
	// drop the old root so only the container root remains visible
	exec.MustRun("[fs] umount old_root", "umount", "-l", "/old_root")

	out, err := exec.RunOut("[net] show IP", "ip", "addr")
	if err != nil {
		panic(fmt.Errorf("ip addr: %w", err))
	}
	fmt.Println(out)
	fmt.Println("PID 1 = ", os.Getpid())

	// replace PID 1 with the container command
	exec.MustRun("[fs] exec cmd", "/bin/bash", "-c", "exec "+cmd)

	os.Exit(0)
}

func waitForVeth(name string) error {
	for i := 0; i < 50; i++ {
		if err := exec.Run("ip", "link", "set", name, "name", "eth0"); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("veth %s not present in netns", name)
}
