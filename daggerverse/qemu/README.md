# qemu

Daggerverse module that boots guest systems under [QEMU](https://www.qemu.org/)
— software (TCG) emulation by default — so embedded firmware and homelab OS
images can be exercised in a pipeline before being flashed to real hardware. One
object model (`Machine`) serves two audiences:

- **Firmware / SBC kernels** — boot a `kernel` (+ optional `dtb` / `initrd` /
  `rootfs`), run to completion (`-no-reboot`), and capture the serial console.
- **Full OS disk images** — boot a disk image as a long-running
  `*dagger.Service` reachable over forwarded TCP ports.

**TCG is the default acceleration.** `KVM` is selectable but requires an engine
exposing `/dev/kvm` (this module does not provision it). The base image is a
module-pinned Alpine (`apk add qemu-system-<arch> qemu-img`); only the
`registry` prefix is caller-overridable, for air-gapped mirrors.

## Enums

Strongly-typed closed sets — invalid values are unrepresentable through the SDK.
The enum value (e.g. `"AARCH64"`) maps to QEMU's lowercase token internally
(`qemu-system-aarch64`).

```go
Arch          // X86_64 I386 AARCH64 ARM RISCV64 RISCV32 MIPS MIPSEL PPC PPC64
Accel         // TCG KVM
DiskFormat    // RAW QCOW2
DiskInterface // VIRTIO IDE SD NVME
```

> Note: the Dagger Go SDK derives each GraphQL enum member from the Go constant
> identifier in `SCREAMING_SNAKE_CASE`, inserting an underscore at every
> letter↔digit boundary, so these surface on the CLI as `AARCH_64`, `RISCV_64`,
> `X_86_64`, `QCOW_2`, etc. The underscores are unavoidable SDK behavior.

## Machine constructors

Both return a `*Machine`. Empty `machine` / `cpu` resolve to the per-arch default
(`AARCH64` => `virt` + `cortex-a53`). Each `tcpPorts` entry is forwarded
`hostfwd=tcp::P-:P` and exposed at the same number. Rejects a nil `kernel` /
nil `image` and an unknown `arch`. Both are `+cache="session"` keyed on `name`
so parallel callers get independent backing services.

```go
Qemu.Linux(
    kernel *dagger.File,
    dtb, initrd, rootfs *dagger.File,   // optional
    arch=AARCH64, machine="", cpu="", memoryMb=512, accel=TCG,
    cmdline="", tcpPorts []int, registry="docker.io", name="",
) (*Machine, error)

Qemu.Disk(
    image *dagger.File,
    arch=AARCH64, machine="", cpu="", memoryMb=1024, accel=TCG,
    format=RAW, iface=VIRTIO, bios *dagger.File, cmdline="",
    tcpPorts []int, registry="docker.io", name="",
) (*Machine, error)
```

## Machine — drive mode A: service-bind

For guests that boot and keep running (e.g. a homelab OS image). Every method is
`+cache="never"`.

```go
Machine.Host() string
Machine.Endpoint(port int) (string, error)   // errors if port wasn't forwarded
Machine.Service() *dagger.Service
Machine.Bind(ctr *dagger.Container) *dagger.Container
Machine.Stop(ctx) error
```

## Machine — drive mode B: run-to-completion

For firmware that runs a test and powers off; captures the serial console. Every
method is `+cache="never"`.

```go
Machine.Run(ctx, timeoutSeconds=300) (string, error)
Machine.WaitForLine(ctx, substr string, timeoutSeconds=300) (string, error)
Machine.SerialLog(ctx, timeoutSeconds=300) (*dagger.File, error)
```

Serial is captured from a finite `WithExec` (`-no-reboot -nographic -serial
mon:stdio`), bounded by `timeout` so a guest that never powers off can't hang.
A per-call nonce busts Dagger's layer cache so each `Run` re-executes QEMU.
`SerialLog` stages the console via `dag.CurrentModule().WorkdirFile`. Input files
are mounted with `WithMountedFile` — no helper containers.

## Follow-ups

Guest-networking integration tests (reaching a forwarded port end-to-end);
bootable disk-image service tests; provisioning `/dev/kvm` for the KVM path.
