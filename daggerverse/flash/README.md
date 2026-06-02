# flash

Codeifies firmware **flashing** as Dagger functions — replacing the usual pile
of shell/Make recipes with composable `dagger call`s. v1 wraps
[probe-rs](https://probe.rs): the physical write is driven from inside Dagger
over USB/IP or probe-rs's remote websocket endpoint, so the whole flash workflow
composes with the [`zig`](../zig) build/objcopy and [`qemu`](../qemu) off-device
test stages.

The one seam that cannot be a hermetic function is presenting the physical USB
probe to the engine — that is a one-time host-side `usbipd` bridge (see
[`BridgeCommand`](#bridgecommand)). Everything downstream of the bus is Dagger.

## Enums

- **`Backend`** — `PROBE_RS` (v1). `OPENOCD`, `ESPTOOL`, `DFU_UTIL` are reserved
  so follow-ups slot in without an API break.
- **`ImageFormat`** — `ELF` (default; probe-rs reads sections directly), `BIN`
  (raw image, **requires** `baseAddress`), `HEX` (Intel HEX).

## Quick start

```sh
# Render the exact probe-rs command without touching hardware:
dagger -m daggerverse/flash call probe-rs \
  --firmware ./firmware.elf --chip STM32F411RETx \
  --remote probe-server:3333 plan

# Inspect a chip against probe-rs's registry:
dagger -m daggerverse/flash call chip-info --chip nrf52840_xxaa

# Emit the host-side USB/IP bridge command (run it on the machine with the probe):
dagger -m daggerverse/flash call bridge-command --busid 3-1
```

Go SDK:

```go
fl := dag.Flash().ProbeRs(firmware, "STM32F411RETx", dagger.FlashProbeRsOpts{
    Usbip: "flash-runner:3240", Busid: "3-1",
})
res := fl.Run()                 // probe-rs download
code, _ := res.ExitCode(ctx)    // 0 on success; non-zero + Output on failure
```

## Functions

| Function | Purpose | Cache |
|---|---|---|
| `ProbeRs(...) → Flasher` | Validate inputs + chip, build the probe-rs container, pick the transport. | `session` |
| `ChipInfo(chip) → string` | Resolve a chip against probe-rs's registry (no probe). | `session` |
| `BridgeCommand(busid, port) → string` | Render the host-side `usbipd`/`usbip` bind command. | — |
| `Flasher.Plan() → string` | Deterministic probe-rs argv — no hardware. | — |
| `Flasher.Run() → FlashResult` | Flash (`probe-rs download`). | `never` |
| `Flasher.Verify() → FlashResult` | Verify on-target flash matches (`probe-rs verify`). | `never` |
| `Flasher.Reset()` | Reset the target (`probe-rs reset`). | `never` |
| `Flasher.GdbServer(port) → Service` | probe-rs GDB stub as a Service (feeds the gdb bridge). | `never` |
| `Flasher.WithServiceBinding(host, svc) → Flasher` | Bind an in-Dagger service (e.g. a usbip relay) under a hostname. | — |

`FlashResult` carries `Output` (combined stdout+stderr) and `ExitCode`. A clean
probe-rs failure (e.g. no probe attached) is reported as a non-zero `ExitCode`
with a **nil error**, so the diagnostic in `Output` is never lost.

## Transports

Exactly **one** transport is required:

- **USB/IP** — `usbip` (`host:port`) + `busid`. The Flasher runs
  `usbip attach -r <host> -b <busid>` inside its container, then probe-rs sees a
  local probe. Present the probe to the engine with `BridgeCommand` first.
- **remote** — `remote` (`host:port`). probe-rs's websocket client connects to a
  remote probe-rs server (`--host`).

There is intentionally **no `*dagger.Socket` transport**: probe-rs speaks raw USB
(CMSIS-DAP HID, ST-Link, J-Link), not a byte stream, so a physical probe cannot
be carried over a unix socket — USB/IP is the only in-container path. A socket
transport will arrive with the serial backends (esptool, dfu-util).

> **`127.0.0.1` inside the container is the container's own loopback, not the
> host.** To reach a host-side `usbipd`, point `usbip` at the host's
> engine-routable address, never `127.0.0.1`.

## probe-rs

The base image is a module-pinned Debian (`bookworm-slim`) with a **pinned**
`probe-rs-tools` release binary (currently `v0.31.0`) and the `usbip` client.
probe-rs's official prebuilt is glibc/libusb-linked, so the base is Debian, not
Alpine. Only a `registry` prefix is caller-overridable (air-gapped mirrors); the
version is bumped by editing `defaultProbeRsVersion`.

<a name="bridgecommand"></a>
## BridgeCommand (host-side USB/IP bridge)

`BridgeCommand` **emits** (does not run) the command that exports a probe to the
engine. Run it on the machine physically holding the probe:

```sh
# On the flash-runner host (Linux usbip tooling):
$(dagger -m daggerverse/flash call bridge-command --busid 3-1)
# Windows equivalent: usbipd bind --busid 3-1   (usbipd-win)
```

## Hardware-in-the-loop (HIL) acceptance — OUT OF CI

The hermetic suite (`tests/All`, run in CI) covers everything **short of the
physical write**: input validation, deterministic `Plan` rendering, chip-registry
resolution against the real probe-rs binary, a clean failure when no probe is
present, and a connection-counting proof that `Run` re-executes (never cached).

A real flash needs a physical probe and is **not gated in CI**. probe-rs drives a
physical debug probe (CMSIS-DAP/ST-Link/J-Link) over SWD/JTAG — **QEMU cannot
serve as a flash target**: QEMU emulates the CPU running the firmware and exposes
a GDB *server*, whereas probe-rs needs a debug *probe* and is itself a GDB
server, not a client. The on-device write is irreducibly HIL.

The hardware-dependent behaviors are codified as runnable functions that carry
**no `+check`** (so CI never runs them) and are **not** in `All`. On a
flash-runner with a probe exported via `usbipd`:

```sh
dagger -m daggerverse/flash/tests call hil-flash-round-trip \
  --firmware ./firmware.elf --chip STM32F411RETx \
  --usbip flash-runner:3240 --busid 3-1

dagger -m daggerverse/flash/tests call hil-verify-matches  --firmware ... --chip ... --usbip ... --busid ...
dagger -m daggerverse/flash/tests call hil-reset           --firmware ... --chip ... --usbip ... --busid ...
dagger -m daggerverse/flash/tests call hil-gdb-serves      --firmware ... --chip ... --usbip ... --busid ...
```

A completed `usbip attach` inside the container needs the `vhci-hcd` kernel
module and a privileged engine; the hermetic tests do not (they only require the
TCP connection to land).

## Tests

`tests/All` is the `+check` hermetic suite. See `tests/main.go` for one example
per function; the `Hil*` functions are the out-of-CI acceptance harness.
