package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"dagger/flash/internal/dagger"
)

const (
	// defaultDebianTag pins the base image. probe-rs's official prebuilt
	// binaries are glibc + libusb linked, so the base must be glibc (Debian),
	// not Alpine/musl. Only the registry prefix is caller-overridable (for
	// air-gapped mirrors); the path and tag are fixed.
	defaultDebianTag = "bookworm-slim"

	// defaultProbeRsVersion pins the probe-rs release whose prebuilt
	// `probe-rs-tools` binary is installed. Bump this (and nothing else) to
	// track upstream. probe-rs is actively maintained on a ~quarterly cadence.
	defaultProbeRsVersion = "0.31.0"

	// firmwarePath is where the firmware payload is mounted; probe-rs reads it
	// as the positional download/verify operand.
	firmwarePath = "/fw/firmware"
	// chipDescPath is where an optional out-of-tree chip-description YAML is
	// mounted for probe-rs's --chip-description-path.
	chipDescPath = "/chip/description.yaml"

	// defaultUsbipPort is the IANA-registered USB/IP TCP port, used when the
	// `usbip` host carries no explicit :port.
	defaultUsbipPort = "3240"
)

// ProbeRs prepares a verified probe-rs flash, reaching the probe over exactly
// one of two transports: USB/IP (`usbip` host:port + `busid`) or a remote
// probe-rs server (`remote` host:port). It validates the inputs, builds the
// probe-rs container, and confirms `chip` against probe-rs's built-in target
// registry, returning a *Flasher whose Run/Verify/Reset/Plan/GdbServer methods
// drive the hardware.
//
// There is intentionally NO *dagger.Socket transport. probe-rs speaks raw USB
// (CMSIS-DAP HID, ST-Link, J-Link), not a byte stream, so a physical probe
// cannot be carried over a unix socket — USB/IP is the only in-container path,
// and the probe is presented to the engine by a host-side usbipd bridge (see
// BridgeCommand). A socket transport will arrive with the serial backends
// (esptool, dfu-util); if you came here hunting for a `--socket` flag, that is
// why there isn't one.
//
// Note on addresses: 127.0.0.1 inside the container is the container's own
// loopback, NOT the host. To reach a host-side usbipd, point `usbip` at the
// host's engine-routable address, never 127.0.0.1.
//
// Session-cached on `name` so parallel callers get independent Flashers; every
// *Flasher method is never-cached so each Run / Verify / Reset re-executes.
// Pass a unique `name` per parallel test for isolation.
//
// +cache="session"
func (f *Flash) ProbeRs(
	ctx context.Context,
	firmware *dagger.File,
	chip string,
	// +default="ELF"
	format ImageFormat,
	// +default=0
	baseAddress int,
	// +default=""
	usbip string,
	// +default=""
	busid string,
	// +default=""
	remote string,
	// +default=""
	probeSelector string,
	// +optional
	chipDescription *dagger.File,
	// +default="docker.io"
	registry string,
	// +default=""
	name string,
) (*Flasher, error) {
	// Cheap, hardware-free validation first, so the rejection paths never pay
	// for the container build. The chip check (which needs probe-rs) is last.
	if firmware == nil {
		return nil, fmt.Errorf("firmware must not be nil; pass a *dagger.File with the firmware image")
	}
	profile, ok := formatTable[format]
	if !ok {
		return nil, fmt.Errorf("unsupported format %q: want ELF, BIN, or HEX", string(format))
	}
	if profile.requiresBaseAddress && baseAddress == 0 {
		return nil, fmt.Errorf("format BIN requires a non-zero base-address (a raw .bin has no load address of its own)")
	}
	hasUsbip := usbip != ""
	hasRemote := remote != ""
	if hasUsbip == hasRemote {
		return nil, fmt.Errorf("exactly one transport is required: set usbip (+busid) or remote, not zero and not both")
	}
	if hasUsbip && busid == "" {
		return nil, fmt.Errorf("usbip transport requires busid (the USB bus id to attach, e.g. 1-1)")
	}

	// ProbeOptions flags shared by every probe-rs subcommand.
	probeArgs := []string{"--chip", chip}
	if probeSelector != "" {
		probeArgs = append(probeArgs, "--probe", probeSelector)
	}
	if chipDescription != nil {
		probeArgs = append(probeArgs, "--chip-description-path", chipDescPath)
	}

	// Transport: usbip attaches the remote device into this container's USB
	// subsystem (so probe-rs then sees a local probe); remote points probe-rs's
	// websocket client at a remote probe-rs server via the global --host flag.
	var attach []string
	if hasRemote {
		probeArgs = append(probeArgs, "--host", remoteWsURL(remote))
	} else {
		host, port := splitHostPort(usbip, defaultUsbipPort)
		attach = []string{"usbip", "--tcp-port", port, "attach", "-r", host, "-b", busid}
	}

	// Format flags apply to download/verify only (reset/gdb ignore them).
	formatArgs := []string{"--binary-format", profile.binaryFormat}
	if format == ImageFormatBin {
		formatArgs = append(formatArgs, "--base-address", fmt.Sprintf("0x%x", baseAddress))
	}

	ctr := baseContainer(registry)
	// Validate the chip against probe-rs's registry (no hardware needed —
	// `chip info` reads the built-in target database).
	if _, err := chipInfo(ctx, ctr, chip); err != nil {
		return nil, err
	}

	ctr = ctr.WithMountedFile(firmwarePath, firmware)
	if chipDescription != nil {
		ctr = ctr.WithMountedFile(chipDescPath, chipDescription)
	}

	host := flasherHost(name, chip, string(format), baseAddress, usbip, busid, remote, probeSelector)
	return &Flasher{
		Ctr:        ctr,
		ProbeArgs:  probeArgs,
		FormatArgs: formatArgs,
		Attach:     attach,
		Hostname:   host,
	}, nil
}

// baseContainer builds the module-pinned Debian with the pinned probe-rs CLI
// and the usbip client installed. Every step is a finite, layer-cached build
// step; the flash exec and the gdb service are launched later from *Flasher.
func baseContainer(registry string) *dagger.Container {
	image := fmt.Sprintf("%s/library/debian:%s", registry, defaultDebianTag)
	dir := "probe-rs-tools-x86_64-unknown-linux-gnu"
	asset := dir + ".tar.xz"
	url := fmt.Sprintf("https://github.com/probe-rs/probe-rs/releases/download/v%s/%s", defaultProbeRsVersion, asset)
	// Download the pinned release, verify its published sha256, extract the
	// `probe-rs` binary (the tarball nests every tool under a top-level dir) to
	// /usr/local/bin. Pinning the asset (not `curl | sh` of latest) keeps the
	// image reproducible.
	install := strings.Join([]string{
		"set -eu",
		"cd /tmp",
		fmt.Sprintf("curl -fsSL -o probe-rs.tar.xz %q", url),
		fmt.Sprintf("curl -fsSL -o probe-rs.tar.xz.sha256 %q", url+".sha256"),
		// The .sha256 file is `<hash>  <filename>`; rewrite the name to match
		// our local file, then verify.
		"awk '{print $1\"  probe-rs.tar.xz\"}' probe-rs.tar.xz.sha256 | sha256sum -c -",
		fmt.Sprintf("tar -xJf probe-rs.tar.xz --strip-components=1 %s/probe-rs", dir),
		"install -m 0755 probe-rs /usr/local/bin/probe-rs",
		"rm -rf probe-rs.tar.xz probe-rs.tar.xz.sha256 probe-rs",
	}, " && ")
	return dag.Container().
		From(image).
		WithExec([]string{"apt-get", "update"}).
		WithExec([]string{"apt-get", "install", "-y", "--no-install-recommends",
			"libusb-1.0-0", "libudev1", "usbip", "ca-certificates", "curl", "xz-utils"}).
		WithExec([]string{"sh", "-c", install})
}

// chipInfo runs `probe-rs chip info <chip>` against probe-rs's built-in target
// registry and returns its output. A non-zero exit (unknown chip) becomes a
// clear error. No probe or hardware is involved.
func chipInfo(ctx context.Context, ctr *dagger.Container, chip string) (string, error) {
	ran := ctr.WithExec([]string{"probe-rs", "chip", "info", chip},
		dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny})
	code, err := ran.ExitCode(ctx)
	if err != nil {
		return "", fmt.Errorf("validate chip %q: %w", chip, err)
	}
	out, err := ran.Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("validate chip %q: %w", chip, err)
	}
	if code != 0 {
		return "", fmt.Errorf("unknown chip %q: not in probe-rs's target registry (run `probe-rs chip list`)", chip)
	}
	return out, nil
}

// ChipInfo resolves a chip against probe-rs's registry and returns the
// human-readable info block, erroring on an unknown chip. It builds the
// module-pinned probe-rs container; no probe or hardware is involved.
//
// +cache="session"
func (f *Flash) ChipInfo(
	ctx context.Context,
	chip string,
	// +default="docker.io"
	registry string,
) (string, error) {
	return chipInfo(ctx, baseContainer(registry), chip)
}

// remoteWsURL turns a host:port (or bare host) into the ws:// URL probe-rs's
// --host flag expects, leaving an explicit ws://, wss://, http://, or https://
// scheme untouched.
func remoteWsURL(remote string) string {
	for _, scheme := range []string{"ws://", "wss://", "http://", "https://"} {
		if strings.HasPrefix(remote, scheme) {
			return remote
		}
	}
	return "ws://" + remote
}

// splitHostPort splits a host:port into its parts, falling back to defaultPort
// when no port is present. IPv6 literals are out of scope for usbip hosts.
func splitHostPort(hostport, defaultPort string) (string, string) {
	if i := strings.LastIndex(hostport, ":"); i >= 0 {
		return hostport[:i], hostport[i+1:]
	}
	return hostport, defaultPort
}

// flasherHost derives a stable, per-configuration hostname so the gdb service
// of two distinct Flashers never collides within one engine session while
// identical-config calls reuse the cached host.
func flasherHost(name, chip, format string, baseAddress int, usbip, busid, remote, probeSelector string) string {
	key := fmt.Appendf(nil, "%s|%s|%s|%d|%s|%s|%s|%s",
		name, chip, format, baseAddress, usbip, busid, remote, probeSelector)
	sum := sha256.Sum256(key)
	return "flash-" + hex.EncodeToString(sum[:6])
}
