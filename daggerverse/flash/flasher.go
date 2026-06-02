package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"dagger/flash/internal/dagger"
)

// Flasher is a prepared-but-not-yet-run probe-rs flash. It carries the
// probe-rs container (with the firmware mounted) and the argv split into the
// pieces each subcommand needs, so Plan can render deterministically without
// hardware while Run / Verify / Reset / GdbServer swap in their verb and exec.
type Flasher struct {
	// +private
	Ctr *dagger.Container
	// +private
	ProbeArgs []string // ProbeOptions flags: --chip, [--probe], [--chip-description-path], [--host]
	// +private
	FormatArgs []string // --binary-format <t> [--base-address 0x..]; download/verify only
	// +private
	Attach []string // usbip-attach prefix (usbip transport), or nil (remote)
	// +private
	Hostname string // stable per-config hostname for the gdb service
}

// FlashResult pairs probe-rs's combined output with its exit code. A clean
// probe-rs failure (e.g. no probe attached) is reported as a non-zero ExitCode
// with a nil Go error, so the captured Output is never lost to the error path.
type FlashResult struct {
	// Output is probe-rs's combined stdout+stderr.
	Output string
	// ExitCode is probe-rs's process exit code (0 = success). A run that
	// exceeds the deadline is killed and yields the timeout code (124).
	ExitCode int
}

// WithServiceBinding binds a service into the Flasher's container under
// `host`, so a usbip or remote transport pointed at that hostname resolves to
// it. This is the seam for reaching an in-Dagger probe relay (e.g. a usbip
// service) — and how the test suite points a Flasher at a fake-usbipd service
// without real hardware. Returns a new Flasher; the original is unchanged.
func (fl *Flasher) WithServiceBinding(host string, svc *dagger.Service) *Flasher {
	return &Flasher{
		Ctr:        fl.Ctr.WithServiceBinding(host, svc),
		ProbeArgs:  fl.ProbeArgs,
		FormatArgs: fl.FormatArgs,
		Attach:     fl.Attach,
		Hostname:   fl.Hostname,
	}
}

// downloadArgv is the probe-rs `download` argv tail (no `probe-rs download`
// prefix): the shared probe options, the format flags, then the firmware
// operand. Verify reuses the same tail under the `verify` verb.
func (fl *Flasher) downloadArgv() []string {
	argv := append([]string{}, fl.ProbeArgs...)
	argv = append(argv, fl.FormatArgs...)
	return append(argv, firmwarePath)
}

// Plan renders the exact probe-rs command the flash (download) path will run —
// deterministic, hardware-free. For the USB/IP transport it includes the
// `usbip attach` prefix; for BIN it includes `--base-address`; for the remote
// transport it has no attach and carries `--host`.
func (fl *Flasher) Plan(ctx context.Context) (string, error) {
	cmd := strings.Join(append([]string{"probe-rs", "download"}, fl.downloadArgv()...), " ")
	if len(fl.Attach) > 0 {
		cmd = strings.Join(fl.Attach, " ") + " && " + cmd
	}
	return cmd, nil
}

// Run flashes the firmware (`probe-rs download`) and returns the combined
// output and exit code. Requires a reachable probe; in CI (no hardware) it
// returns a non-zero ExitCode with the probe-rs error in Output.
//
// +cache="never"
func (fl *Flasher) Run(
	ctx context.Context,
	// +default=120
	timeoutSeconds int,
) (*FlashResult, error) {
	return fl.runProbe(ctx, append([]string{"download"}, fl.downloadArgv()...), timeoutSeconds)
}

// Verify checks that the on-target flash matches the firmware
// (`probe-rs verify`) without rewriting it. Requires a reachable probe.
//
// +cache="never"
func (fl *Flasher) Verify(
	ctx context.Context,
	// +default=120
	timeoutSeconds int,
) (*FlashResult, error) {
	return fl.runProbe(ctx, append([]string{"verify"}, fl.downloadArgv()...), timeoutSeconds)
}

// Reset resets the target (`probe-rs reset`). Requires a reachable probe; a
// non-zero probe-rs exit becomes an error here (Reset has no FlashResult).
//
// +cache="never"
func (fl *Flasher) Reset(
	ctx context.Context,
	// +default=120
	timeoutSeconds int,
) error {
	res, err := fl.runProbe(ctx, append([]string{"reset"}, fl.ProbeArgs...), timeoutSeconds)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("probe-rs reset failed (exit %d):\n%s", res.ExitCode, res.Output)
	}
	return nil
}

// GdbServer exposes probe-rs's GDB stub as a Service so a debugger (or the gdb
// bridge, #106) can attach over the network. Requires a reachable probe to
// serve a live target; HIL-only at runtime. For the USB/IP transport the
// attach runs first inside the service command.
//
// +cache="never"
func (fl *Flasher) GdbServer(
	// +default=1337
	port int,
) *dagger.Service {
	gdb := append([]string{"probe-rs", "gdb"}, fl.ProbeArgs...)
	gdb = append(gdb, "--gdb-connection-string", fmt.Sprintf("0.0.0.0:%d", port))
	var args []string
	if len(fl.Attach) > 0 {
		args = []string{"sh", "-c", strings.Join(fl.Attach, " ") + "; exec " + strings.Join(gdb, " ")}
	} else {
		args = gdb
	}
	return fl.Ctr.
		WithExposedPort(port).
		AsService(dagger.ContainerAsServiceOpts{Args: args}).
		WithHostname(fl.Hostname)
}

// runProbe executes one probe-rs subcommand to completion and returns its
// combined output and exit code.
//
// A per-call random nonce env var busts Dagger's inner WithExec layer cache so
// each call actually re-runs probe-rs (the +cache="never" on the method governs
// the function result, not the inner exec). probe-rs logs to stderr, so the
// command redirects 2>&1 — without it a failing run's Output would be empty.
//
// `timeout -s KILL N` bounds a hung probe-rs, and Expect=Any tolerates the
// resulting non-zero exit so the output and code both survive. The SIGKILL exit
// (137) is rewritten to the conventional timeout code (124) inside the sh -c
// wrapper: Expect=Any treats signal-kill exits in 128-191 as exec errors, which
// would make Stdout fail and discard the captured output (the qemu runSerial
// gotcha).
func (fl *Flasher) runProbe(ctx context.Context, probeArgv []string, timeoutSeconds int) (*FlashResult, error) {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 120
	}
	nonce, err := randHex()
	if err != nil {
		return nil, err
	}
	inner := "probe-rs " + strings.Join(probeArgv, " ")
	if len(fl.Attach) > 0 {
		inner = strings.Join(fl.Attach, " ") + " && " + inner
	}
	inner += " 2>&1"
	// $0 is the timeout in seconds; $1 is the inner command. 137 = 128 + SIGKILL.
	const wrap = `timeout -s KILL "$0" sh -c "$1"; ec=$?; [ "$ec" -eq 137 ] && ec=124; exit "$ec"`
	args := []string{"sh", "-c", wrap, strconv.Itoa(timeoutSeconds), inner}
	ran := fl.Ctr.
		WithEnvVariable("FLASH_RUN_NONCE", nonce).
		WithExec(args, dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny})
	out, err := ran.Stdout(ctx)
	if err != nil {
		return nil, fmt.Errorf("run probe-rs: %w", err)
	}
	code, err := ran.ExitCode(ctx)
	if err != nil {
		return nil, fmt.Errorf("run probe-rs: read exit code: %w", err)
	}
	return &FlashResult{Output: out, ExitCode: code}, nil
}

// randHex returns 32 hex chars of cryptographically-random data for use as a
// cache-busting nonce.
func randHex() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}
