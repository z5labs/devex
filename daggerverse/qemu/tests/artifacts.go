package main

import (
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
