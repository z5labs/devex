package main

import (
	"fmt"
	"strings"

	"dagger/tests/internal/dagger"
)

const (
	alpineTestTag = "3.21"
	// alpineNetboot is the CDN path to Alpine's aarch64 netboot artifacts —
	// a prebuilt `linux-virt` kernel + matching initramfs that boot cleanly
	// under qemu-system-aarch64 -M virt. Fetched at test time so no binaries
	// are committed to the repo.
	alpineNetboot = "https://dl-cdn.alpinelinux.org/alpine/v" + alpineTestTag + "/releases/aarch64/netboot"
)

// alpineArm64Kernel fetches the real Alpine aarch64 `vmlinuz-virt` kernel as a
// *dagger.File — the genuine distro kernel the firmware and boot tests boot.
func alpineArm64Kernel() *dagger.File {
	return curlFile(alpineNetboot+"/vmlinuz-virt", "vmlinuz-virt")
}

// curlFile downloads url into a freshly-pinned Alpine container and returns it
// as a *dagger.File named out.
func curlFile(url, out string) *dagger.File {
	return dag.Container().
		From("alpine:"+alpineTestTag).
		WithExec([]string{"apk", "add", "--no-cache", "curl"}).
		WithExec([]string{"curl", "-fsSL", "-o", "/" + out, url}).
		File("/" + out)
}

// alpineArm64NetModules extracts the kernel modules the virtio networking
// stack needs into a flat *dagger.Directory of plain (decompressed) .ko files.
// Alpine's aarch64 `linux-virt` kernel builds virtio_net, the -M virt MMIO
// transport, and virtio_net's failover deps as modules (=m), so the busybox
// initramfs has to insmod them before any eth0 exists. modloop-virt is pulled
// from the same netboot release as the kernel, so the module versions match
// vmlinuz-virt exactly. Modules are found by name (not a hardcoded version
// path) so a 3.21 point-release still resolves, and normalized to plain .ko so
// the guest's busybox insmod can load them without compression support.
func alpineArm64NetModules() *dagger.Directory {
	return dag.Container().
		From("alpine:"+alpineTestTag).
		WithExec([]string{"apk", "add", "--no-cache", "curl", "squashfs-tools", "zstd"}).
		WithExec([]string{"curl", "-fsSL", "-o", "/modloop", alpineNetboot + "/modloop-virt"}).
		WithExec([]string{"sh", "-c", `set -eu
unsquashfs -d /sqsh /modloop
mkdir /mods
for m in failover net_failover virtio_mmio virtio_net; do
  f=$(find /sqsh \( -name "$m.ko" -o -name "$m.ko.gz" -o -name "$m.ko.zst" \) | head -n1)
  if [ -z "$f" ]; then echo "module $m not found in modloop" >&2; exit 1; fi
  case "$f" in
    *.gz)  gunzip -c "$f" > /mods/$m.ko ;;
    *.zst) unzstd -c "$f" > /mods/$m.ko ;;
    *)     cp "$f" /mods/$m.ko ;;
  esac
done`}).
		Directory("/mods")
}

// serviceInitramfs builds a gzip-compressed cpio initramfs whose /init brings
// up guest networking and runs a persistent TCP listener on port — the
// service-bind counterpart to firmwareInitramfs. It bundles the virtio net
// modules at /lib/modules, insmods them in dependency order, assigns slirp's
// default guest address (10.0.2.15, the hostfwd target), then loops a busybox
// `nc -l` so the forwarded port survives repeated probes (busybox nc serves one
// connection per invocation). The busybox is the aarch64 static build, matching
// the aarch64 kernel.
func serviceInitramfs(port int) *dagger.File {
	busybox := dag.Container(dagger.ContainerOpts{Platform: dagger.Platform("linux/arm64")}).
		From("alpine:"+alpineTestTag).
		WithExec([]string{"apk", "add", "--no-cache", "busybox-static"}).
		File("/bin/busybox.static")

	init := strings.Join([]string{
		"#!/bin/busybox sh",
		"/bin/busybox mkdir -p /proc /sys /dev /tmp",
		"/bin/busybox mount -t proc none /proc",
		"/bin/busybox mount -t sysfs none /sys",
		"/bin/busybox mount -t devtmpfs none /dev",
		// linux-virt builds the virtio net stack as modules (=m); load in dep
		// order: failover <- net_failover <- virtio_net, plus the -M virt MMIO
		// transport. virtio core/PCI are built-in (=y).
		"/bin/busybox insmod /lib/modules/failover.ko",
		"/bin/busybox insmod /lib/modules/net_failover.ko",
		"/bin/busybox insmod /lib/modules/virtio_mmio.ko",
		"/bin/busybox insmod /lib/modules/virtio_net.ko",
		// Wait for the NIC to probe before configuring it.
		"i=0; while [ $i -lt 30 ]; do /bin/busybox ifconfig eth0 >/dev/null 2>&1 && break; /bin/busybox sleep 1; i=$((i+1)); done",
		"/bin/busybox ifconfig lo 127.0.0.1 up",
		"/bin/busybox ifconfig eth0 10.0.2.15 netmask 255.255.255.0 up",
		"/bin/busybox route add default gw 10.0.2.2",
		// Prime QEMU slirp: a listen-only guest never sends outbound traffic, so
		// slirp never learns the guest and inbound hostfwd silently fails to
		// deliver (connect is accepted host-side but no data reaches the guest).
		// One outbound packet to the gateway registers the guest so hostfwd works.
		"/bin/busybox ping -c 2 -W 2 10.0.2.2 >/dev/null 2>&1 || true",
		// Mint a per-boot identity token so a consumer can tell one boot of this
		// machine from another: a fresh boot (e.g. after Stop tears the VM down
		// and a rebind restarts it) serves a different token. Stable within one
		// boot, so two reads of the same running instance match.
		"/bin/busybox head -c 16 /dev/urandom | /bin/busybox od -An -tx1 | /bin/busybox tr -d ' \\n' > /tmp/token",
		fmt.Sprintf("/bin/busybox echo LISTENER_UP %d", port),
		// Serve the token with a one-shot listener restarted by the outer loop:
		// `nc -l` copies stdin (/tmp/token) to the connected socket, then exits
		// when the client disconnects, so the client reads token+EOF and the next
		// loop iteration relistens for the next probe. busybox nc has no `-k`
		// keep-listening, and its `-e` execs a single program *path* with no
		// applet args (`-e /bin/busybox cat` would exec busybox with no applet
		// selected and never serve the token), so feeding the file via stdin is
		// the reliable way to serve it. Note: a slirp hostfwd port accepts
		// host-side even before the guest listens, so only an actual data read
		// (the token) proves end-to-end reachability — a bare connect would be a
		// false positive.
		fmt.Sprintf("while true; do /bin/busybox nc -l -p %d < /tmp/token; done", port),
		"",
	}, "\n")

	return dag.Container().
		From("alpine:"+alpineTestTag).
		WithExec([]string{"apk", "add", "--no-cache", "cpio"}).
		WithMountedFile("/src/busybox", busybox).
		WithMountedDirectory("/src/mods", alpineArm64NetModules()).
		WithNewFile("/src/init", init).
		WithExec([]string{"sh", "-c",
			"set -e; mkdir -p /root/bin /root/lib/modules; " +
				"cp /src/busybox /root/bin/busybox; chmod +x /root/bin/busybox; " +
				"cp /src/mods/*.ko /root/lib/modules/; " +
				"cp /src/init /root/init; chmod +x /root/init; " +
				"cd /root && find . | cpio -o -H newc | gzip -9 > /initramfs.gz"}).
		File("/initramfs.gz")
}

// firmwareInitramfs builds a tiny gzip-compressed cpio initramfs whose /init
// (1) prints marker to the serial console, (2) emits 16 random bytes from
// /dev/urandom as hex (so two boots differ — proving Run is not cached), then
// (3) powers the guest off via sysrq so the finite QEMU run exits. The busybox
// inside is the aarch64 static build, matching the aarch64 kernel.
func firmwareInitramfs(marker string) *dagger.File {
	busybox := dag.Container(dagger.ContainerOpts{Platform: dagger.Platform("linux/arm64")}).
		From("alpine:"+alpineTestTag).
		WithExec([]string{"apk", "add", "--no-cache", "busybox-static"}).
		File("/bin/busybox.static")

	init := strings.Join([]string{
		"#!/bin/busybox sh",
		"/bin/busybox mkdir -p /proc /sys /dev",
		"/bin/busybox mount -t proc none /proc",
		"/bin/busybox mount -t devtmpfs none /dev",
		"/bin/busybox echo " + marker,
		// Prove we reached a functional userspace (working syscalls), not just
		// the kernel's own early boot log: uname is run from PID 1 here.
		"/bin/busybox echo USERSPACE_OK $(/bin/busybox uname -sr)",
		"/bin/busybox head -c 16 /dev/urandom | /bin/busybox od -An -tx1 | /bin/busybox tr -d ' \\n'",
		"/bin/busybox echo",
		"/bin/busybox echo FIRMWARE_DONE",
		"/bin/busybox sync",
		"/bin/busybox echo o > /proc/sysrq-trigger",
		"/bin/busybox poweroff -f",
		"",
	}, "\n")

	return dag.Container().
		From("alpine:"+alpineTestTag).
		WithExec([]string{"apk", "add", "--no-cache", "cpio"}).
		WithMountedFile("/src/busybox", busybox).
		WithNewFile("/src/init", init).
		WithExec([]string{"sh", "-c",
			"set -e; mkdir -p /root/bin; " +
				"cp /src/busybox /root/bin/busybox; chmod +x /root/bin/busybox; " +
				"cp /src/init /root/init; chmod +x /root/init; " +
				"cd /root && find . | cpio -o -H newc | gzip -9 > /initramfs.gz"}).
		File("/initramfs.gz")
}
