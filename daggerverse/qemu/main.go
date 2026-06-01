// Qemu provides Dagger functions for booting guest systems under QEMU —
// software (TCG) emulation by default — so embedded firmware and homelab
// OS images can be exercised in a pipeline before being flashed to real
// hardware. One object model serves two audiences: microcontroller / SBC
// firmware (boot a kernel + rootfs and watch the serial console) and full
// server/SBC OS images (boot a disk image as a long-running service and
// reach it over forwarded ports).
//
// TCG is the default acceleration; KVM is selectable but requires an engine
// with /dev/kvm (this module does not provide it). The base image is a
// module-pinned Alpine (`apk add qemu-system-<arch> qemu-img`); only a
// `registry` prefix is caller-overridable, for air-gapped mirrors.
//
// File map (all `package main`, surfaced as one Dagger module):
//
//   - enums.go    — Arch / Accel / DiskFormat / DiskInterface enums plus the
//                   internal per-arch tables (qemu binary, apk package,
//                   default machine/cpu, net front-end) and token mappings.
//   - machine.go  — *Machine + Qemu.Linux / Qemu.Disk constructors, input
//                   validation, the QEMU argv builder, and per-arch defaults.
//   - drive.go    — the two *Machine drive modes: service-bind (Host /
//                   Endpoint / Service / Bind / Stop) and run-to-completion
//                   (Run / WaitForLine / SerialLog), plus the workdir stager.
package main

// Qemu is the root namespace for every exported function in this module.
// The machine constructors hang off *Qemu so the generated Dagger SDK
// surfaces them under `dag.Qemu().<Func>(...)`.
type Qemu struct{}
