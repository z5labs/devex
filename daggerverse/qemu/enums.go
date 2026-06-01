package main

import "strings"

// Arch is the guest CPU architecture QEMU emulates. The enum value (e.g.
// "AARCH64") maps to QEMU's lowercase token internally (`qemu-system-aarch64`);
// invalid architectures are unrepresentable through the SDK.
//
// Note on rendered names: the Dagger Go SDK derives each GraphQL enum member
// from the *constant identifier* in SCREAMING_SNAKE_CASE and inserts an
// underscore at every letter↔digit boundary, so these surface as `AARCH_64`,
// `RISCV_64`, `X_86_64`, etc. The underscores are unavoidable SDK behavior; the
// internal Value() (the string literal below) is what drives the qemu mapping.
type Arch string

const (
	ArchX86_64  Arch = "X86_64"
	ArchI386    Arch = "I386"
	ArchAarch64 Arch = "AARCH64"
	ArchArm     Arch = "ARM"
	ArchRiscv64 Arch = "RISCV64"
	ArchRiscv32 Arch = "RISCV32"
	ArchMips    Arch = "MIPS"
	ArchMipsel  Arch = "MIPSEL"
	ArchPpc     Arch = "PPC"
	ArchPpc64   Arch = "PPC64"
)

// Accel is the QEMU acceleration mode. TCG is pure-software emulation and
// needs no host support; KVM requires an engine exposing /dev/kvm, which
// this module does not provision.
type Accel string

const (
	AccelTcg Accel = "TCG"
	AccelKvm Accel = "KVM"
)

// DiskFormat is the on-disk image format passed to `-drive format=`.
type DiskFormat string

const (
	DiskFormatRaw   DiskFormat = "RAW"
	DiskFormatQcow2 DiskFormat = "QCOW2"
)

// DiskInterface is the guest-visible disk bus passed to `-drive if=`.
type DiskInterface string

const (
	DiskInterfaceVirtio DiskInterface = "VIRTIO"
	DiskInterfaceIde    DiskInterface = "IDE"
	DiskInterfaceSd     DiskInterface = "SD"
	DiskInterfaceNvme   DiskInterface = "NVME"
)

// archProfile holds the per-arch facts the argv builder needs: the default
// `-M` machine and `-cpu` model when the caller leaves them empty, and the
// virtio net front-end appropriate to that machine family.
type archProfile struct {
	defaultMachine string
	defaultCPU     string
	netDevice      string
}

// archTable enumerates every supported architecture. Membership doubles as
// the "is this a known arch with a qemu-system-<arch> package" check; an
// Arch absent from this map is rejected by the constructors.
var archTable = map[Arch]archProfile{
	ArchAarch64: {defaultMachine: "virt", defaultCPU: "cortex-a53", netDevice: "virtio-net-device"},
	ArchArm:     {defaultMachine: "virt", defaultCPU: "cortex-a15", netDevice: "virtio-net-device"},
	ArchX86_64:  {defaultMachine: "q35", defaultCPU: "", netDevice: "virtio-net-pci"},
	ArchI386:    {defaultMachine: "pc", defaultCPU: "", netDevice: "virtio-net-pci"},
	ArchRiscv64: {defaultMachine: "virt", defaultCPU: "", netDevice: "virtio-net-device"},
	ArchRiscv32: {defaultMachine: "virt", defaultCPU: "", netDevice: "virtio-net-device"},
	ArchMips:    {defaultMachine: "malta", defaultCPU: "", netDevice: "virtio-net-pci"},
	ArchMipsel:  {defaultMachine: "malta", defaultCPU: "", netDevice: "virtio-net-pci"},
	ArchPpc:     {defaultMachine: "g3beige", defaultCPU: "", netDevice: "virtio-net-pci"},
	ArchPpc64:   {defaultMachine: "pseries", defaultCPU: "", netDevice: "virtio-net-pci"},
}

// mcuTable holds the MCU-class machine/cpu defaults for the bare-metal path,
// deliberately distinct from archTable's SoC defaults (virt/q35): a Cortex-M
// firmware boots on a microcontroller board, not a virt machine. Membership
// doubles as the "is this a supported bare-metal arch" check; an Arch absent
// from this map is rejected by BareMetal. netDevice is unused here — the
// bare-metal path has no networking — so it is left empty.
var mcuTable = map[Arch]archProfile{
	ArchArm:     {defaultMachine: "lm3s6965evb", defaultCPU: "cortex-m3"},
	ArchRiscv64: {defaultMachine: "virt", defaultCPU: ""},
	ArchRiscv32: {defaultMachine: "virt", defaultCPU: ""},
}

// qemuSystem returns the `qemu-system-<arch>` binary name (which is also the
// apk package name) for this Arch — the uppercase enum value lowercased.
func (a Arch) qemuSystem() string {
	return "qemu-system-" + strings.ToLower(string(a))
}

// token returns the lowercase QEMU `-accel` token for this mode.
func (a Accel) token() string { return strings.ToLower(string(a)) }

// token returns the lowercase QEMU `-drive format=` token for this format.
func (f DiskFormat) token() string { return strings.ToLower(string(f)) }

// token returns the lowercase QEMU `-drive if=` token for this interface.
func (i DiskInterface) token() string { return strings.ToLower(string(i)) }
