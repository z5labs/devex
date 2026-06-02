// Flash provides Dagger functions that codeify firmware flashing — replacing the
// usual pile of shell/Make recipes with composable `dagger call`s. v1 wraps
// probe-rs: the physical write is driven from inside Dagger over USB/IP (a probe
// exported from the host with usbipd) or probe-rs's remote websocket endpoint,
// so flashing composes with the zig (#108) build/objcopy and qemu (#107)
// off-device test stages.
//
// The one seam that cannot be a hermetic function is presenting the physical USB
// probe to the engine — that is a one-time host-side usbipd bridge (USB/IP),
// runner setup rather than a per-project script. Everything downstream of the
// bus is Dagger; BridgeCommand codeifies even that setup as emitted output.
//
// File map (all `package main`, surfaced as one Dagger module):
//
//   - enums.go    — Backend / ImageFormat enums plus the format token table.
//   - probers.go  — Flash.ProbeRs factory: input validation, the probe-rs base
//                   container, chip-registry validation, and the argv builder.
//   - flasher.go  — *Flasher + its drive methods (Plan / Run / Verify / Reset /
//                   GdbServer), FlashResult, and the run-to-completion exec
//                   helper that captures output + exit code.
package main

import "fmt"

// Flash is the root namespace for every exported function in this module. The
// ProbeRs factory hangs off *Flash so the generated SDK surfaces it under
// `dag.Flash().ProbeRs(...)`.
type Flash struct{}

// BridgeCommand emits — it does NOT run — the host-side USB/IP command that
// exports a probe to the engine, codeifying the one-time runner setup as output.
// This runs on the machine physically holding the probe, OUTSIDE Dagger: it
// binds `busid` to the USB/IP host driver and starts usbipd listening on
// `port`, after which a Flasher built with `usbip=<host>:<port>` and the same
// `busid` can attach the probe.
//
// 127.0.0.1 inside a container is the container's own loopback, not the host —
// point the Flasher's `usbip` at the host's engine-routable address, and run
// this command on the host.
//
// The rendered form is the Linux usbip/usbipd toolchain. On Windows the
// equivalent is `usbipd bind --busid <busid>` followed by `usbipd attach`
// from the engine side (usbipd-win); see the module README.
func (f *Flash) BridgeCommand(
	busid string,
	// +default="3240"
	port string,
) string {
	return fmt.Sprintf("usbipd -tcp-port %s & usbip bind --busid %s", port, busid)
}
