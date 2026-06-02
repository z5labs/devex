package main

// Backend selects the underlying flashing tool. v1 ships PROBE_RS; the rest are
// reserved so follow-ups (serial backends, openocd) slot in without an API
// break. ProbeRs is a dedicated factory, so the enum is not taken as a
// construction argument yet — it exists to keep the surface stable.
//
// SDK note: the Dagger Go SDK derives each GraphQL enum member from the Go
// constant *identifier* in SCREAMING_SNAKE_CASE, inserting an underscore at
// every letter<->digit boundary. These have no digits, so they surface
// unchanged (PROBE_RS, OPENOCD, ESPTOOL, DFU_UTIL); the same machinery turns
// qemu's ArchX86_64 into X_86_64. The string literal below is the internal
// value.
type Backend string

const (
	BackendProbeRs Backend = "PROBE_RS"
	BackendOpenocd Backend = "OPENOCD"  // reserved (v1 does not implement)
	BackendEsptool Backend = "ESPTOOL"  // reserved
	BackendDfuUtil Backend = "DFU_UTIL" // reserved
)

// ImageFormat is how the firmware payload is encoded (pairs with zig #108's
// ObjCopy output). ELF carries its own load addresses and is the default;
// BIN is headerless and REQUIRES a baseAddress; HEX is Intel HEX and, like
// ELF, carries addresses.
type ImageFormat string

const (
	ImageFormatElf ImageFormat = "ELF" // default; probe-rs reads sections directly
	ImageFormatBin ImageFormat = "BIN" // raw; requires baseAddress
	ImageFormatHex ImageFormat = "HEX" // Intel HEX
)

// formatProfile pairs each ImageFormat with the probe-rs `--binary-format`
// token and whether it needs an explicit base address. Membership in
// formatTable doubles as the "is this a known format" check.
type formatProfile struct {
	binaryFormat        string // probe-rs --binary-format token
	requiresBaseAddress bool
}

var formatTable = map[ImageFormat]formatProfile{
	ImageFormatElf: {binaryFormat: "elf", requiresBaseAddress: false},
	ImageFormatBin: {binaryFormat: "bin", requiresBaseAddress: true},
	ImageFormatHex: {binaryFormat: "hex", requiresBaseAddress: false},
}
