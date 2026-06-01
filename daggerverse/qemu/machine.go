package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"dagger/qemu/internal/dagger"
)

const (
	// defaultAlpineTag pins the base image. Only the registry prefix is
	// caller-overridable (air-gapped mirrors); the `library/alpine` path and
	// this tag are fixed so every machine boots from a known toolchain.
	defaultAlpineTag = "3.21"

	kernelPath = "/boot/qemu-kernel"
	initrdPath = "/boot/qemu-initrd"
	dtbPath    = "/boot/qemu.dtb"
	rootfsPath = "/boot/qemu-rootfs.img"
	diskPath   = "/disk/qemu-image.img"
	biosPath   = "/fw/qemu-bios.bin"
)

// Machine is a configured-but-not-necessarily-running QEMU guest. It carries
// both drive modes: a long-running *dagger.Service (mode A, for OS images
// reached over forwarded ports) and a finite run argv replayed per call (mode
// B, for firmware that prints to serial and powers off). See drive.go.
type Machine struct {
	// +private
	Base *dagger.Container
	// +private
	Svc *dagger.Service
	// +private
	Hostname string
	// +private
	RunArgv []string
	// +private
	Ports []int
}

// Linux boots a guest from a raw kernel (plus optional dtb / initrd / rootfs)
// — the firmware / SBC path. `arch` selects the qemu-system-<arch> binary;
// empty `machine` / `cpu` resolve to the per-arch default (AARCH64 => virt +
// cortex-a53). Each `tcpPorts` entry is forwarded `hostfwd=tcp::P-:P` and
// exposed at the same number. Rejects a nil kernel and an unknown arch.
//
// Session-cached on `name` so parallel callers get independent backing
// services; every *Machine method is never-cached so each Run / Service
// re-executes. Pass a unique `name` per parallel test for isolation.
//
// +cache="session"
func (q *Qemu) Linux(
	ctx context.Context,
	kernel *dagger.File,
	// +optional
	dtb *dagger.File,
	// +optional
	initrd *dagger.File,
	// +optional
	rootfs *dagger.File,
	// +default="AARCH64"
	arch Arch,
	// +default=""
	machine string,
	// +default=""
	cpu string,
	// +default=512
	memoryMb int,
	// +default="TCG"
	accel Accel,
	// +default=""
	cmdline string,
	// +optional
	tcpPorts []int,
	// +default="docker.io"
	registry string,
	// +default=""
	name string,
) (*Machine, error) {
	if kernel == nil {
		return nil, fmt.Errorf("kernel must not be nil; pass a *dagger.File with the guest kernel image")
	}
	profile, ok := archTable[arch]
	if !ok {
		return nil, fmt.Errorf("unsupported arch %q: no qemu-system-%s package", string(arch), strings.ToLower(string(arch)))
	}
	machine, cpu = resolveMachineCPU(profile, machine, cpu)

	base := baseContainer(arch, registry).WithMountedFile(kernelPath, kernel)
	core := baseArgv(arch, machine, cpu, memoryMb, accel)
	core = append(core, "-kernel", kernelPath)
	if initrd != nil {
		base = base.WithMountedFile(initrdPath, initrd)
		core = append(core, "-initrd", initrdPath)
	}
	if dtb != nil {
		base = base.WithMountedFile(dtbPath, dtb)
		core = append(core, "-dtb", dtbPath)
	}
	if rootfs != nil {
		base = base.WithMountedFile(rootfsPath, rootfs)
		core = append(core, "-drive", "file="+rootfsPath+",format=raw,if=virtio")
	}
	if cmdline != "" {
		core = append(core, "-append", cmdline)
	}
	core = appendNet(core, profile, tcpPorts)

	host := machineHost("linux", name, string(arch), machine, cpu, memoryMb, accel, cmdline, tcpPorts)
	return assembleMachine(base, core, host, tcpPorts), nil
}

// Disk boots a guest from a bootable disk image as a long-running machine —
// the full-OS path. `format` / `iface` map to `-drive format=`/`if=`; an
// optional `bios` supplies firmware. Empty `machine` / `cpu` resolve to the
// per-arch default. Rejects a nil image and an unknown arch.
//
// +cache="session"
func (q *Qemu) Disk(
	ctx context.Context,
	image *dagger.File,
	// +default="AARCH64"
	arch Arch,
	// +default=""
	machine string,
	// +default=""
	cpu string,
	// +default=1024
	memoryMb int,
	// +default="TCG"
	accel Accel,
	// +default="RAW"
	format DiskFormat,
	// +default="VIRTIO"
	iface DiskInterface,
	// +optional
	bios *dagger.File,
	// +default=""
	cmdline string,
	// +optional
	tcpPorts []int,
	// +default="docker.io"
	registry string,
	// +default=""
	name string,
) (*Machine, error) {
	if image == nil {
		return nil, fmt.Errorf("image must not be nil; pass a *dagger.File with the bootable disk image")
	}
	profile, ok := archTable[arch]
	if !ok {
		return nil, fmt.Errorf("unsupported arch %q: no qemu-system-%s package", string(arch), strings.ToLower(string(arch)))
	}
	machine, cpu = resolveMachineCPU(profile, machine, cpu)

	base := baseContainer(arch, registry).WithMountedFile(diskPath, image)
	core := baseArgv(arch, machine, cpu, memoryMb, accel)
	core = append(core, "-drive", fmt.Sprintf("file=%s,format=%s,if=%s", diskPath, format.token(), iface.token()))
	if bios != nil {
		base = base.WithMountedFile(biosPath, bios)
		core = append(core, "-bios", biosPath)
	}
	core = appendNet(core, profile, tcpPorts)

	host := machineHost("disk", name, string(arch), machine, cpu, memoryMb, accel, cmdline, tcpPorts)
	return assembleMachine(base, core, host, tcpPorts), nil
}

// baseContainer builds the module-pinned Alpine with the per-arch
// qemu-system-<arch> binary and qemu-img installed. `apk add` is a finite
// build step (layer-cached); the long-running guest is launched later via
// AsService / a finite WithExec, never here.
func baseContainer(arch Arch, registry string) *dagger.Container {
	image := fmt.Sprintf("%s/library/alpine:%s", registry, defaultAlpineTag)
	return dag.Container().
		From(image).
		WithExec([]string{"apk", "add", "--no-cache", arch.qemuSystem(), "qemu-img"})
}

// baseArgv is the argv prefix common to both constructors and both drive
// modes: binary, machine, memory, accel, and (when set) cpu.
func baseArgv(arch Arch, machine, cpu string, memoryMb int, accel Accel) []string {
	argv := []string{arch.qemuSystem(), "-M", machine, "-m", strconv.Itoa(memoryMb), "-accel", accel.token()}
	if cpu != "" {
		argv = append(argv, "-cpu", cpu)
	}
	return argv
}

// resolveMachineCPU fills empty machine / cpu with the per-arch defaults.
func resolveMachineCPU(p archProfile, machine, cpu string) (string, string) {
	if machine == "" {
		machine = p.defaultMachine
	}
	if cpu == "" {
		cpu = p.defaultCPU
	}
	return machine, cpu
}

// appendNet adds a user-mode netdev with one hostfwd per port plus the
// arch-appropriate virtio net front-end. No ports => no networking.
func appendNet(core []string, p archProfile, ports []int) []string {
	if len(ports) == 0 {
		return core
	}
	netdev := "user,id=net0"
	for _, port := range ports {
		netdev += fmt.Sprintf(",hostfwd=tcp::%d-:%d", port, port)
	}
	return append(core, "-netdev", netdev, "-device", p.netDevice+",netdev=net0")
}

// assembleMachine derives the two drive forms from the shared core argv: the
// long-running service (mode A; `-display none`, ports exposed) and the
// finite run argv (mode B; `-no-reboot -nographic -serial mon:stdio`).
func assembleMachine(base *dagger.Container, core []string, host string, ports []int) *Machine {
	serviceArgv := append(append([]string{}, core...), "-display", "none")
	svcCtr := base
	for _, port := range ports {
		svcCtr = svcCtr.WithExposedPort(port)
	}
	svc := svcCtr.AsService(dagger.ContainerAsServiceOpts{Args: serviceArgv}).WithHostname(host)

	runArgv := append(append([]string{}, core...), "-no-reboot", "-nographic", "-serial", "mon:stdio")

	return &Machine{
		Base:     base,
		Svc:      svc,
		Hostname: host,
		RunArgv:  runArgv,
		Ports:    ports,
	}
}

// machineHost derives a stable, per-configuration hostname. The suffix hashes
// every arg that distinguishes one session-cache entry from another, so two
// distinct machines never collide on a hostname within one engine session
// while identical-arg calls reuse the cached machine.
func machineHost(mode, name, arch, machine, cpu string, memoryMb int, accel Accel, cmdline string, ports []int) string {
	key := fmt.Appendf(nil, "%s|%s|%s|%s|%s|%d|%s|%s|%v",
		mode, name, arch, machine, cpu, memoryMb, string(accel), cmdline, ports)
	sum := sha256.Sum256(key)
	return "qemu-" + hex.EncodeToString(sum[:6]) // 12 hex chars = 48 bits
}
