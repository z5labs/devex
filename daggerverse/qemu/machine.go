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
	// firmwarePath is where a bare-metal firmware ELF/bin is mounted for the
	// BareMetal path; QEMU loads it via -kernel and boots it directly with no
	// Linux kernel, initrd, rootfs, or device tree.
	firmwarePath = "/fw/qemu-firmware.elf"
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

// BareMetal boots a bare-metal microcontroller firmware directly — no Linux
// kernel, initrd, rootfs, or device tree — on an MCU-class machine, with
// ARM/RISC-V semihosting enabled. This is the off-device unit-test path for
// embedded firmware: a test build can write to the host console (semihosting
// SYS_WRITE0) and report pass/fail as a process exit code (semihosting
// SYS_EXIT) via Machine.RunStatus — neither of which the kernel-oriented Linux
// path can surface (it captures serial text only).
//
// `arch` selects the qemu-system-<arch> binary and an MCU-class machine
// default distinct from the SoC defaults Linux/Disk use (ARM => lm3s6965evb +
// cortex-m3, RISC-V => virt); an explicit `machine` / `cpu` overrides it.
// Acceleration is always TCG — MCU targets have no KVM analog. Semihosting is
// on by default (`-semihosting-config enable=on,target=native` lets the guest's
// semihosting calls reach the host so SYS_EXIT maps to the QEMU process exit
// code); pass `disableSemihosting` for firmware that drives a real UART instead
// and wants neither the host console route nor SYS_EXIT wiring. The option is
// inverted so the Go SDK can actually turn semihosting off — a `+default=true`
// bool can't be set false through the generated bindings (false is the zero
// value and is dropped). Rejects a nil firmware and an arch with no bare-metal
// profile.
//
// Session-cached on `name` like Linux/Disk; every *Machine method is
// never-cached so each Run / RunStatus re-executes.
//
// +cache="session"
func (q *Qemu) BareMetal(
	ctx context.Context,
	firmware *dagger.File,
	// +default="ARM"
	arch Arch,
	// +default=""
	machine string,
	// +default=""
	cpu string,
	// +default=16
	memoryMb int,
	// +default=false
	disableSemihosting bool,
	// +default=""
	cmdline string,
	// +default="docker.io"
	registry string,
	// +default=""
	name string,
) (*Machine, error) {
	if firmware == nil {
		return nil, fmt.Errorf("firmware must not be nil; pass a *dagger.File with the bare-metal firmware image")
	}
	profile, ok := mcuTable[arch]
	if !ok {
		return nil, fmt.Errorf("unsupported bare-metal arch %q: BareMetal supports ARM (lm3s6965evb) and RISCV64/RISCV32 (virt)", string(arch))
	}
	machine, cpu = resolveMachineCPU(profile, machine, cpu)

	base := baseContainer(arch, registry).WithMountedFile(firmwarePath, firmware)
	core := baseArgv(arch, machine, cpu, memoryMb, AccelTcg)
	core = append(core, "-kernel", firmwarePath)
	if !disableSemihosting {
		// Route the semihosting console (SYS_WRITE0) to QEMU's own stdout via a
		// /dev/stdout file chardev, not its default sink (stderr) — the
		// run-to-completion path captures stdout, so without this the guest's
		// semihosting output would be invisible to Run / RunStatus. target=native
		// makes SYS_EXIT map to the QEMU process exit code that RunStatus reads.
		core = append(core,
			"-chardev", "file,id=semi0,path=/dev/stdout",
			"-semihosting-config", "enable=on,target=native,chardev=semi0")
	}
	if cmdline != "" {
		core = append(core, "-append", cmdline)
	}

	host := machineHost("baremetal", name, string(arch), machine, cpu, memoryMb, AccelTcg, cmdline, nil)
	return assembleMachine(base, core, host, nil), nil
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
