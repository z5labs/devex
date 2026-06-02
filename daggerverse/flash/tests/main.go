// Tests for the flash daggerverse module. Each test is a standalone dagger
// function so it can be invoked individually during TDD; All wires the hermetic
// suite up for parallel execution and carries `+check` so CI schedules it.
//
// The hermetic suite never touches real hardware: it covers input validation,
// deterministic Plan rendering, chip-registry resolution (the real probe-rs
// binary, no probe), a clean failure when no probe is present, and a
// connection-counting proof that Run re-executes (never cached). The genuinely
// hardware-dependent behaviors are the Hil* functions below — they carry NO
// `+check` and are NOT in All, so CI never runs them; a flash-runner operator
// invokes them against a real probe.
package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	par "github.com/dagger/dagger/util/parallel"

	"dagger/tests/internal/dagger"
)

type Tests struct{}

// knownChip is a chip guaranteed to be in probe-rs's built-in target registry
// (it appears in probe-rs's own test suite), used by the resolve/render tests.
const knownChip = "nrf52840_xxaa"

// All runs the hermetic flash suite in parallel. It carries `+check` so CI
// schedules it; the Hil* functions are deliberately excluded (no hardware in
// CI).
//
// +check
// +cache="session"
func (t *Tests) All(
	ctx context.Context,
	// +default=0
	parallel int,
) error {
	jobs := par.New().
		WithRollupLogs(true).
		WithRollupSpans(true)
	if parallel > 0 {
		jobs = jobs.WithLimit(parallel)
	}
	jobs = jobs.WithJob("ProbeRsRejectsNilFirmware", t.ProbeRsRejectsNilFirmware)
	jobs = jobs.WithJob("ProbeRsRejectsUnknownChip", t.ProbeRsRejectsUnknownChip)
	jobs = jobs.WithJob("ProbeRsRejectsBinWithoutBaseAddress", t.ProbeRsRejectsBinWithoutBaseAddress)
	jobs = jobs.WithJob("ProbeRsRejectsNoTransport", t.ProbeRsRejectsNoTransport)
	jobs = jobs.WithJob("ProbeRsRejectsBothTransports", t.ProbeRsRejectsBothTransports)
	jobs = jobs.WithJob("ProbeRsRejectsUsbipWithoutBusid", t.ProbeRsRejectsUsbipWithoutBusid)
	jobs = jobs.WithJob("PlanRendersElfDownload", t.PlanRendersElfDownload)
	jobs = jobs.WithJob("PlanRendersBinBaseAddress", t.PlanRendersBinBaseAddress)
	jobs = jobs.WithJob("PlanReflectsRemoteTransport", t.PlanReflectsRemoteTransport)
	jobs = jobs.WithJob("FlasherHasProbeRs", t.FlasherHasProbeRs)
	jobs = jobs.WithJob("ChipInfoResolvesKnownChip", t.ChipInfoResolvesKnownChip)
	jobs = jobs.WithJob("RunWithoutProbeFailsCleanly", t.RunWithoutProbeFailsCleanly)
	jobs = jobs.WithJob("BridgeCommandRendersUsbipBind", t.BridgeCommandRendersUsbipBind)
	jobs = jobs.WithJob("RunReExecutesNotCached", t.RunReExecutesNotCached)
	return jobs.Run(ctx)
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// randName returns a short hex-suffixed identifier for use as a per-test
// Flasher `name` (folds into ProbeRs's session-cache key) or a cache-busting
// nonce.
func randName(ctx context.Context, prefix string) (string, error) {
	h, err := dag.Random().Sha256(ctx, dagger.RandomSha256Opts{N: 16})
	if err != nil {
		return "", err
	}
	return prefix + h[:12], nil
}

// randPort mints an unprivileged TCP port at runtime so parallel service tests
// never collide.
func randPort(ctx context.Context) (int, error) {
	h, err := dag.Random().Sha256(ctx, dagger.RandomSha256Opts{N: 8})
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(h[:4], 16, 32)
	if err != nil {
		return 0, err
	}
	return 20000 + int(v)%40000, nil
}

// placeholderFile returns a throwaway *dagger.File for tests that never flash.
func placeholderFile() *dagger.File {
	return dag.Directory().WithNewFile("firmware", "flash-test-firmware").File("firmware")
}

// -----------------------------------------------------------------------------
// Validation tests (no container)
// -----------------------------------------------------------------------------

// ProbeRsRejectsNilFirmware verifies a nil firmware is rejected — either by the
// SDK binding's assertNotNil panic or by ProbeRs's own up-front check.
func (t *Tests) ProbeRsRejectsNilFirmware(ctx context.Context) (returnErr error) {
	defer func() {
		if r := recover(); r != nil && !strings.Contains(fmt.Sprint(r), "firmware") {
			returnErr = fmt.Errorf("expected panic to mention firmware, got: %v", r)
		}
	}()
	_, err := dag.Flash().ProbeRs(nil, knownChip, dagger.FlashProbeRsOpts{Remote: "127.0.0.1:9"}).Plan(ctx)
	if err == nil {
		return fmt.Errorf("expected nil firmware to be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "firmware") {
		return fmt.Errorf("expected firmware-required error, got: %v", err)
	}
	return nil
}

// ProbeRsRejectsBinWithoutBaseAddress verifies BIN with baseAddress=0 is rejected.
func (t *Tests) ProbeRsRejectsBinWithoutBaseAddress(ctx context.Context) error {
	_, err := dag.Flash().ProbeRs(placeholderFile(), knownChip, dagger.FlashProbeRsOpts{
		Format: dagger.FlashImageFormatBin,
		Remote: "127.0.0.1:9",
	}).Plan(ctx)
	if err == nil {
		return fmt.Errorf("expected BIN without baseAddress to be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "base-address") {
		return fmt.Errorf("expected base-address error, got: %v", err)
	}
	return nil
}

// ProbeRsRejectsNoTransport verifies zero transports is rejected.
func (t *Tests) ProbeRsRejectsNoTransport(ctx context.Context) error {
	_, err := dag.Flash().ProbeRs(placeholderFile(), knownChip, dagger.FlashProbeRsOpts{}).Plan(ctx)
	if err == nil {
		return fmt.Errorf("expected no transport to be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "exactly one transport") {
		return fmt.Errorf("expected transport error, got: %v", err)
	}
	return nil
}

// ProbeRsRejectsBothTransports verifies setting both usbip and remote is rejected.
func (t *Tests) ProbeRsRejectsBothTransports(ctx context.Context) error {
	_, err := dag.Flash().ProbeRs(placeholderFile(), knownChip, dagger.FlashProbeRsOpts{
		Usbip:  "host:3240",
		Busid:  "1-1",
		Remote: "127.0.0.1:9",
	}).Plan(ctx)
	if err == nil {
		return fmt.Errorf("expected both transports to be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "exactly one transport") {
		return fmt.Errorf("expected transport error, got: %v", err)
	}
	return nil
}

// ProbeRsRejectsUsbipWithoutBusid verifies usbip without busid is rejected.
func (t *Tests) ProbeRsRejectsUsbipWithoutBusid(ctx context.Context) error {
	_, err := dag.Flash().ProbeRs(placeholderFile(), knownChip, dagger.FlashProbeRsOpts{
		Usbip: "host:3240",
	}).Plan(ctx)
	if err == nil {
		return fmt.Errorf("expected usbip without busid to be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "busid") {
		return fmt.Errorf("expected busid error, got: %v", err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Plan render + chip tests (build the probe-rs container)
// -----------------------------------------------------------------------------

// PlanRendersElfDownload asserts the ELF + usbip plan renders the probe-rs
// download argv with the usbip-attach prefix and no --base-address.
func (t *Tests) PlanRendersElfDownload(ctx context.Context) error {
	name, err := randName(ctx, "elf-")
	if err != nil {
		return err
	}
	plan, err := dag.Flash().ProbeRs(placeholderFile(), knownChip, dagger.FlashProbeRsOpts{
		Usbip: "probe-host:3240",
		Busid: "1-1",
		Name:  name,
	}).Plan(ctx)
	if err != nil {
		return fmt.Errorf("Plan: %w", err)
	}
	for _, want := range []string{"usbip", "attach", "-b 1-1", "probe-rs download", "--chip " + knownChip, "/fw/firmware"} {
		if !strings.Contains(plan, want) {
			return fmt.Errorf("expected plan to contain %q, got:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "--base-address") {
		return fmt.Errorf("expected ELF plan to omit --base-address, got:\n%s", plan)
	}
	return nil
}

// PlanRendersBinBaseAddress asserts the BIN plan renders --base-address with the
// hex address.
func (t *Tests) PlanRendersBinBaseAddress(ctx context.Context) error {
	name, err := randName(ctx, "bin-")
	if err != nil {
		return err
	}
	plan, err := dag.Flash().ProbeRs(placeholderFile(), knownChip, dagger.FlashProbeRsOpts{
		Format:      dagger.FlashImageFormatBin,
		BaseAddress: 0x8000000,
		Usbip:       "probe-host:3240",
		Busid:       "1-1",
		Name:        name,
	}).Plan(ctx)
	if err != nil {
		return fmt.Errorf("Plan: %w", err)
	}
	for _, want := range []string{"--binary-format bin", "--base-address 0x8000000"} {
		if !strings.Contains(plan, want) {
			return fmt.Errorf("expected plan to contain %q, got:\n%s", want, plan)
		}
	}
	return nil
}

// PlanReflectsRemoteTransport asserts the remote plan carries the remote value
// and has no usbip-attach prefix. It asserts on the remote value, not the exact
// flag spelling, so it stays robust to probe-rs CLI evolution.
func (t *Tests) PlanReflectsRemoteTransport(ctx context.Context) error {
	name, err := randName(ctx, "rem-")
	if err != nil {
		return err
	}
	plan, err := dag.Flash().ProbeRs(placeholderFile(), knownChip, dagger.FlashProbeRsOpts{
		Remote: "probe-server:3333",
		Name:   name,
	}).Plan(ctx)
	if err != nil {
		return fmt.Errorf("Plan: %w", err)
	}
	if !strings.Contains(plan, "probe-server:3333") {
		return fmt.Errorf("expected plan to reflect remote endpoint, got:\n%s", plan)
	}
	if strings.Contains(plan, "usbip attach") {
		return fmt.Errorf("expected remote plan to omit usbip attach, got:\n%s", plan)
	}
	return nil
}

// FlasherHasProbeRs asserts ProbeRs wires a probe-rs invocation (the flash
// backend is present), via a successful Plan render mentioning probe-rs.
func (t *Tests) FlasherHasProbeRs(ctx context.Context) error {
	name, err := randName(ctx, "has-")
	if err != nil {
		return err
	}
	plan, err := dag.Flash().ProbeRs(placeholderFile(), knownChip, dagger.FlashProbeRsOpts{
		Remote: "probe-server:3333",
		Name:   name,
	}).Plan(ctx)
	if err != nil {
		return fmt.Errorf("Plan: %w", err)
	}
	if !strings.Contains(plan, "probe-rs") {
		return fmt.Errorf("expected the flash backend to be probe-rs, got:\n%s", plan)
	}
	return nil
}

// ProbeRsRejectsUnknownChip verifies an unknown chip is rejected against
// probe-rs's registry (this runs the real probe-rs binary).
func (t *Tests) ProbeRsRejectsUnknownChip(ctx context.Context) error {
	_, err := dag.Flash().ProbeRs(placeholderFile(), "NOT_A_REAL_CHIP_XYZ", dagger.FlashProbeRsOpts{
		Remote: "probe-server:3333",
	}).Plan(ctx)
	if err == nil {
		return fmt.Errorf("expected unknown chip to be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "unknown chip") {
		return fmt.Errorf("expected unknown-chip error, got: %v", err)
	}
	return nil
}

// ChipInfoResolvesKnownChip verifies a known chip resolves against probe-rs's
// registry and the info block mentions the chip family.
func (t *Tests) ChipInfoResolvesKnownChip(ctx context.Context) error {
	info, err := dag.Flash().ChipInfo(ctx, knownChip)
	if err != nil {
		return fmt.Errorf("ChipInfo(%q): %w", knownChip, err)
	}
	if !strings.Contains(strings.ToLower(info), "nrf52840") {
		return fmt.Errorf("expected chip info to mention nrf52840, got:\n%s", info)
	}
	return nil
}

// RunWithoutProbeFailsCleanly verifies that with no reachable probe, Run returns
// a non-zero exit code and captured output — NOT a Go error (the FlashResult
// failure contract). Points at the container's own loopback where nothing
// listens, so probe-rs fails fast.
func (t *Tests) RunWithoutProbeFailsCleanly(ctx context.Context) error {
	name, err := randName(ctx, "noprobe-")
	if err != nil {
		return err
	}
	res := dag.Flash().ProbeRs(placeholderFile(), knownChip, dagger.FlashProbeRsOpts{
		Remote: "127.0.0.1:9",
		Name:   name,
	}).Run(dagger.FlashFlasherRunOpts{TimeoutSeconds: 60})
	code, err := res.ExitCode(ctx)
	if err != nil {
		return fmt.Errorf("Run should fail cleanly (no Go error), got: %w", err)
	}
	if code == 0 {
		return fmt.Errorf("expected a non-zero exit code without a probe, got 0")
	}
	out, err := res.Output(ctx)
	if err != nil {
		return fmt.Errorf("read output: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("expected probe-rs to emit a diagnostic, got empty output")
	}
	return nil
}

// BridgeCommandRendersUsbipBind verifies BridgeCommand renders a host-side
// usbip bind command carrying the busid and port.
func (t *Tests) BridgeCommandRendersUsbipBind(ctx context.Context) error {
	cmd, err := dag.Flash().BridgeCommand(ctx, "3-1", dagger.FlashBridgeCommandOpts{Port: "4040"})
	if err != nil {
		return fmt.Errorf("BridgeCommand: %w", err)
	}
	for _, want := range []string{"bind", "3-1", "4040"} {
		if !strings.Contains(cmd, want) {
			return fmt.Errorf("expected bridge command to contain %q, got: %q", want, cmd)
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// Cache-never proof (counting fake-usbipd service)
// -----------------------------------------------------------------------------

// countingUsbipd builds a TCP service that increments a counter on every
// inbound connection and returns the running count. probe-rs's usbip attach
// connects to it (and fails to import — no real device), but the TCP connection
// still lands and counts; reading the count out-of-band lets a test prove two
// Run calls each re-execute.
func countingUsbipd(port int) *dagger.Service {
	script := fmt.Sprintf(`import socketserver, threading
count = 0
lock = threading.Lock()
class H(socketserver.BaseRequestHandler):
    def handle(self):
        global count
        with lock:
            count += 1
            n = count
        try:
            self.request.sendall(("%%d\n" %% n).encode())
        except Exception:
            pass
class S(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True
S(("0.0.0.0", %d), H).serve_forever()
`, port)
	return dag.Container().
		From("python:3-alpine").
		WithExposedPort(port).
		AsService(dagger.ContainerAsServiceOpts{Args: []string{"python", "-u", "-c", script}})
}

// readUsbipdCount connects once to the counting service and returns the count
// it reports (the connection itself increments the counter). A per-read nonce
// keeps two reads from cache-colliding.
func readUsbipdCount(ctx context.Context, svc *dagger.Service, host string, port int) (int, error) {
	nonce, err := randName(ctx, "")
	if err != nil {
		return 0, err
	}
	out, err := dag.Container().
		From("alpine:3.21").
		WithServiceBinding(host, svc).
		WithEnvVariable("READ_NONCE", nonce).
		WithExec([]string{"sh", "-c", fmt.Sprintf("nc -w 5 %s %d </dev/null | tr -dc '0-9'", host, port)}).
		Stdout(ctx)
	if err != nil {
		return 0, fmt.Errorf("read usbipd count: %w", err)
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

// RunReExecutesNotCached proves Run's never-cache behavior by counting how many
// times two Run calls hit a fake-usbipd service. Each Run's usbip attach opens one TCP
// connection; reading the counter before and after the two Runs, the
// Run-attributable connection delta must be exactly 2 (a cached Run would skip
// the connect and yield fewer).
func (t *Tests) RunReExecutesNotCached(ctx context.Context) error {
	port, err := randPort(ctx)
	if err != nil {
		return err
	}
	const host = "fake-usbipd"
	svc := countingUsbipd(port)
	if _, err := svc.Start(ctx); err != nil {
		return fmt.Errorf("start counting service: %w", err)
	}
	defer svc.Stop(ctx)

	name, err := randName(ctx, "nocache-")
	if err != nil {
		return err
	}
	fl := dag.Flash().ProbeRs(placeholderFile(), knownChip, dagger.FlashProbeRsOpts{
		Usbip: fmt.Sprintf("%s:%d", host, port),
		Busid: "1-1",
		Name:  name,
	}).WithServiceBinding(host, svc)

	before, err := readUsbipdCount(ctx, svc, host, port)
	if err != nil {
		return err
	}
	// Two independent Run calls; each must re-execute and connect.
	for i := 0; i < 2; i++ {
		if _, err := fl.Run(dagger.FlashFlasherRunOpts{TimeoutSeconds: 60}).ExitCode(ctx); err != nil {
			return fmt.Errorf("run %d: %w", i+1, err)
		}
	}
	after, err := readUsbipdCount(ctx, svc, host, port)
	if err != nil {
		return err
	}
	// after - before counts: the two Run connects + this final read itself.
	runConnects := (after - before) - 1
	if runConnects != 2 {
		return fmt.Errorf("expected 2 Run-attributable connections (uncached), got %d (before=%d after=%d)", runConnects, before, after)
	}
	return nil
}

// -----------------------------------------------------------------------------
// HIL acceptance — NO +check, NOT in All. Run on a flash-runner with a real
// probe exported via usbipd (see README). CI never schedules these.
// -----------------------------------------------------------------------------

// HilFlashRoundTrip flashes firmware to a real target and verifies it, asserting
// both succeed. Provide a usbip host:port + busid (from BridgeCommand) or a
// remote endpoint, and the chip.
func (t *Tests) HilFlashRoundTrip(
	ctx context.Context,
	firmware *dagger.File,
	chip string,
	// +default=""
	usbip string,
	// +default=""
	busid string,
	// +default=""
	remote string,
	// +default="ELF"
	format string,
	// +default=0
	baseAddress int,
) error {
	fl := dag.Flash().ProbeRs(firmware, chip, dagger.FlashProbeRsOpts{
		Format: dagger.FlashImageFormat(format), BaseAddress: baseAddress, Usbip: usbip, Busid: busid, Remote: remote,
	})
	run := fl.Run()
	if code, err := run.ExitCode(ctx); err != nil {
		return err
	} else if code != 0 {
		out, _ := run.Output(ctx)
		return fmt.Errorf("flash failed (exit %d):\n%s", code, out)
	}
	ver := fl.Verify()
	if code, err := ver.ExitCode(ctx); err != nil {
		return err
	} else if code != 0 {
		out, _ := ver.Output(ctx)
		return fmt.Errorf("verify failed (exit %d):\n%s", code, out)
	}
	return nil
}

// HilVerifyMatches verifies the on-target flash matches firmware without
// rewriting it.
func (t *Tests) HilVerifyMatches(
	ctx context.Context,
	firmware *dagger.File,
	chip string,
	// +default=""
	usbip string,
	// +default=""
	busid string,
	// +default=""
	remote string,
) error {
	res := dag.Flash().ProbeRs(firmware, chip, dagger.FlashProbeRsOpts{
		Usbip: usbip, Busid: busid, Remote: remote,
	}).Verify()
	code, err := res.ExitCode(ctx)
	if err != nil {
		return err
	}
	if code != 0 {
		out, _ := res.Output(ctx)
		return fmt.Errorf("verify mismatch (exit %d):\n%s", code, out)
	}
	return nil
}

// HilReset resets a real target.
func (t *Tests) HilReset(
	ctx context.Context,
	firmware *dagger.File,
	chip string,
	// +default=""
	usbip string,
	// +default=""
	busid string,
	// +default=""
	remote string,
) error {
	return dag.Flash().ProbeRs(firmware, chip, dagger.FlashProbeRsOpts{
		Usbip: usbip, Busid: busid, Remote: remote,
	}).Reset(ctx)
}

// HilGdbServes starts the probe-rs GDB stub against a real target and asserts a
// gdb client can reach it over the bound service.
func (t *Tests) HilGdbServes(
	ctx context.Context,
	firmware *dagger.File,
	chip string,
	// +default=""
	usbip string,
	// +default=""
	busid string,
	// +default=""
	remote string,
	// +default=1337
	port int,
) error {
	svc := dag.Flash().ProbeRs(firmware, chip, dagger.FlashProbeRsOpts{
		Usbip: usbip, Busid: busid, Remote: remote,
	}).GdbServer(dagger.FlashFlasherGdbServerOpts{Port: port})
	out, err := dag.Container().
		From("alpine:3.21").
		WithServiceBinding("gdb", svc).
		WithExec([]string{"sh", "-c", fmt.Sprintf("nc -z -w 10 gdb %d && echo SERVING", port)}).
		Stdout(ctx)
	if err != nil {
		return fmt.Errorf("gdb server not reachable: %w", err)
	}
	if !strings.Contains(out, "SERVING") {
		return fmt.Errorf("expected gdb stub to be serving on %d", port)
	}
	return nil
}
