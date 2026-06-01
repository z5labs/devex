// Tests for the qemu daggerverse module. Each test is exposed as a standalone
// dagger function so it can be invoked individually during TDD; All wires them
// up for parallel execution under `dagger call all`. The four sub-aggregators
// (Validation, Firmware, Boot, Networking) each carry `+check` so CI schedules
// them onto their own runners.
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

// All runs every qemu test as a convenience for local `dagger call all`
// invocations. CI does NOT call All: each sub-aggregator below carries its own
// `+check` directive, so GH Actions schedules each onto its own runner in
// parallel — running All on top would double-bill the same work.
//
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
	jobs = jobs.WithJob("Validation", func(ctx context.Context) error { return t.Validation(ctx, parallel) })
	jobs = jobs.WithJob("Firmware", func(ctx context.Context) error { return t.Firmware(ctx, parallel) })
	jobs = jobs.WithJob("Boot", func(ctx context.Context) error { return t.Boot(ctx, parallel) })
	jobs = jobs.WithJob("Networking", func(ctx context.Context) error { return t.Networking(ctx, parallel) })
	return jobs.Run(ctx)
}

// Validation runs the pure input-rejection and accessor tests. None boot a
// guest, so they're fast and safe to fan out unbounded.
//
// +check
// +cache="session"
func (t *Tests) Validation(
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
	jobs = jobs.WithJob("linux-rejects-nil-kernel", t.LinuxRejectsNilKernel)
	jobs = jobs.WithJob("disk-rejects-nil-image", t.DiskRejectsNilImage)
	jobs = jobs.WithJob("endpoint-rejects-unforwarded-port", t.EndpointRejectsUnforwardedPort)
	jobs = jobs.WithJob("endpoint-returns-forwarded-host-port", t.EndpointReturnsForwardedHostPort)
	return jobs.Run(ctx)
}

// Firmware runs the run-to-completion serial-capture tests against a tiny
// custom-init initramfs (fast under TCG).
//
// +check
// +cache="session"
func (t *Tests) Firmware(
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
	jobs = jobs.WithJob("run-captures-firmware-serial", t.RunCapturesFirmwareSerial)
	jobs = jobs.WithJob("wait-for-line-matches-firmware-serial", t.WaitForLineMatchesFirmwareSerial)
	jobs = jobs.WithJob("serial-log-materializes-file", t.SerialLogMaterializesFile)
	jobs = jobs.WithJob("run-should-not-be-cached", t.RunShouldNotBeCached)
	return jobs.Run(ctx)
}

// Boot runs the real-kernel boot tests against the Alpine aarch64 netboot
// kernel (slow TCG boots).
//
// +check
// +cache="session"
func (t *Tests) Boot(
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
	jobs = jobs.WithJob("defaults-boot-arm-64-kernel", t.DefaultsBootArm64Kernel)
	jobs = jobs.WithJob("linux-boot-reaches-userspace", t.LinuxBootReachesUserspace)
	return jobs.Run(ctx)
}

// Networking runs the service-bind end-to-end tests: boot a guest that brings up
// networking and serves a forwarded port, then prove reachability and teardown
// through WithServiceBinding. These are slow TCG boots plus real networking.
//
// +check
// +cache="session"
func (t *Tests) Networking(
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
	jobs = jobs.WithJob("service-forwarded-port-reachable", t.ServiceForwardedPortReachable)
	jobs = jobs.WithJob("stop-halts-service", t.StopHaltsService)
	return jobs.Run(ctx)
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// randName returns a short hex-suffixed identifier for use as a per-test
// machine `name`, which folds into Qemu.Linux/Disk's +cache="session" key so
// parallel tests get independent backing machines.
func randName(ctx context.Context, prefix string) (string, error) {
	h, err := dag.Random().Sha256(ctx, dagger.RandomSha256Opts{N: 16})
	if err != nil {
		return "", err
	}
	return prefix + h[:12], nil
}

// placeholderFile returns a throwaway *dagger.File for accessor/rejection
// tests that never actually boot the guest.
func placeholderFile(name string) *dagger.File {
	return dag.Directory().WithNewFile(name, "qemu-test-placeholder").File(name)
}

// randPort mints an unprivileged TCP port at runtime from random bytes (no
// literals committed) so parallel service tests never collide on a forwarded
// port. The value folds into the guest listener, the tcpPorts hostfwd, and the
// consumer probe alike.
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

// serviceMachine builds an aarch64 Machine that boots the real Alpine kernel
// with the service initramfs: it brings up networking and listens on port. label
// keeps parallel tests on independent session-cached machines.
func serviceMachine(ctx context.Context, label string, port int) (*dagger.QemuMachine, error) {
	tok, err := randName(ctx, "")
	if err != nil {
		return nil, err
	}
	return dag.Qemu().Linux(alpineArm64Kernel(), dagger.QemuLinuxOpts{
		Initrd:   serviceInitramfs(port),
		Cmdline:  "console=ttyAMA0 rdinit=/init",
		TCPPorts: []int{port},
		Name:     "qemu-svc-" + label + "-" + tok,
	}), nil
}

// reachToken binds the machine into a fresh consumer and reads the guest's
// per-boot identity token off the forwarded port, retrying up to attempts
// seconds. The returned token identifies which boot answered, so a caller can
// tell a torn-down-and-restarted instance from one that stayed up. The nonce
// env var keeps two reads against the same host:port from cache-colliding.
func reachToken(ctx context.Context, m *dagger.QemuMachine, port, attempts int) (string, error) {
	host, err := m.Host(ctx)
	if err != nil {
		return "", err
	}
	nonce, err := randName(ctx, "")
	if err != nil {
		return "", err
	}
	bound := m.Bind(dag.Container().
		From("alpine:" + alpineTestTag).
		WithExec([]string{"apk", "add", "--no-cache", "busybox-extras"}))
	// Hold the client's stdin open with a sleep so busybox nc doesn't close the
	// socket on stdin-EOF before it reads the token the guest sends; head -c
	// returns as soon as the token arrives. Each attempt is ~3s, so no extra
	// sleep between iterations.
	out, err := bound.
		WithEnvVariable("REACH_NONCE", nonce).
		WithExec([]string{"sh", "-c", fmt.Sprintf(
			"for i in $(seq 1 %d); do out=$( (sleep 3) | nc -w 3 %s %d 2>/dev/null | tr -dc 'a-f0-9' | head -c 64 ); "+
				"if [ -n \"$out\" ]; then printf '%%s' \"$out\"; exit 0; fi; done; exit 1",
			attempts, host, port)}).
		Stdout(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// -----------------------------------------------------------------------------
// Validation tests
// -----------------------------------------------------------------------------

// LinuxRejectsNilKernel verifies a nil kernel is rejected. The Dagger SDK
// binding panics via assertNotNil before the call leaves the test module;
// recover and assert the panic mentions the rejected argument.
//
// +cache="never"
func (t *Tests) LinuxRejectsNilKernel(ctx context.Context) (returnErr error) {
	defer func() {
		r := recover()
		if r == nil {
			returnErr = fmt.Errorf("expected Linux(nil) to panic via assertNotNil, but it did not")
			return
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "kernel") {
			returnErr = fmt.Errorf("expected panic to mention kernel, got: %v", r)
		}
	}()
	m := dag.Qemu().Linux(nil)
	_, _ = m.Host(ctx)
	return nil
}

// DiskRejectsNilImage verifies a nil disk image is rejected (assertNotNil
// panic, as above).
//
// +cache="never"
func (t *Tests) DiskRejectsNilImage(ctx context.Context) (returnErr error) {
	defer func() {
		r := recover()
		if r == nil {
			returnErr = fmt.Errorf("expected Disk(nil) to panic via assertNotNil, but it did not")
			return
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "image") {
			returnErr = fmt.Errorf("expected panic to mention image, got: %v", r)
		}
	}()
	m := dag.Qemu().Disk(nil)
	_, _ = m.Host(ctx)
	return nil
}

// EndpointRejectsUnforwardedPort verifies Endpoint errors for a port that was
// not in the machine's tcpPorts.
//
// +cache="never"
func (t *Tests) EndpointRejectsUnforwardedPort(ctx context.Context) error {
	name, err := randName(ctx, "qemu-")
	if err != nil {
		return err
	}
	m := dag.Qemu().Linux(placeholderFile("kernel"), dagger.QemuLinuxOpts{
		TCPPorts: []int{8080},
		Name:     name,
	})
	if _, err := m.Endpoint(ctx, 9090); err == nil {
		return fmt.Errorf("expected Endpoint(9090) to fail for an unforwarded port, got nil error")
	} else if !strings.Contains(err.Error(), "not forwarded") {
		return fmt.Errorf("expected error to mention 'not forwarded', got: %v", err)
	}
	return nil
}

// EndpointReturnsForwardedHostPort verifies Endpoint returns host:port for a
// forwarded port.
//
// +cache="never"
func (t *Tests) EndpointReturnsForwardedHostPort(ctx context.Context) error {
	name, err := randName(ctx, "qemu-")
	if err != nil {
		return err
	}
	m := dag.Qemu().Linux(placeholderFile("kernel"), dagger.QemuLinuxOpts{
		TCPPorts: []int{8080},
		Name:     name,
	})
	host, err := m.Host(ctx)
	if err != nil {
		return err
	}
	ep, err := m.Endpoint(ctx, 8080)
	if err != nil {
		return err
	}
	if want := host + ":8080"; ep != want {
		return fmt.Errorf("expected endpoint %q, got %q", want, ep)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Firmware (run-to-completion serial capture) tests
// -----------------------------------------------------------------------------

// firmwareMachine builds an aarch64 Machine that boots the Alpine kernel with a
// tiny custom initramfs which prints marker (plus random bytes) to serial and
// powers off. label keeps parallel tests on independent session-cached machines.
func firmwareMachine(ctx context.Context, label string) (machine *dagger.QemuMachine, marker string, err error) {
	tok, err := randName(ctx, "")
	if err != nil {
		return nil, "", err
	}
	marker = "QEMUFWMARKER" + tok
	machine = dag.Qemu().Linux(alpineArm64Kernel(), dagger.QemuLinuxOpts{
		Initrd:  firmwareInitramfs(marker),
		Cmdline: "console=ttyAMA0 rdinit=/init",
		Name:    "qemu-fw-" + label + "-" + tok,
	})
	return machine, marker, nil
}

// RunCapturesFirmwareSerial verifies Run boots the firmware to completion and
// returns its serial console.
//
// +cache="never"
func (t *Tests) RunCapturesFirmwareSerial(ctx context.Context) error {
	m, marker, err := firmwareMachine(ctx, "run")
	if err != nil {
		return err
	}
	out, err := m.Run(ctx, dagger.QemuMachineRunOpts{TimeoutSeconds: 180})
	if err != nil {
		return err
	}
	if !strings.Contains(out, marker) {
		return fmt.Errorf("expected serial console to contain marker %q, got:\n%s", marker, out)
	}
	if !strings.Contains(out, "FIRMWARE_DONE") {
		return fmt.Errorf("expected serial console to contain FIRMWARE_DONE, got:\n%s", out)
	}
	return nil
}

// WaitForLineMatchesFirmwareSerial verifies WaitForLine returns the console
// once the marker line appears.
//
// +cache="never"
func (t *Tests) WaitForLineMatchesFirmwareSerial(ctx context.Context) error {
	m, marker, err := firmwareMachine(ctx, "waitforline")
	if err != nil {
		return err
	}
	out, err := m.WaitForLine(ctx, marker, dagger.QemuMachineWaitForLineOpts{TimeoutSeconds: 180})
	if err != nil {
		return err
	}
	if !strings.Contains(out, marker) {
		return fmt.Errorf("expected returned console to contain marker %q, got:\n%s", marker, out)
	}
	return nil
}

// SerialLogMaterializesFile verifies SerialLog stages the serial console as a
// readable *dagger.File.
//
// +cache="never"
func (t *Tests) SerialLogMaterializesFile(ctx context.Context) error {
	m, marker, err := firmwareMachine(ctx, "seriallog")
	if err != nil {
		return err
	}
	contents, err := m.SerialLog(dagger.QemuMachineSerialLogOpts{TimeoutSeconds: 180}).Contents(ctx)
	if err != nil {
		return err
	}
	if !strings.Contains(contents, marker) {
		return fmt.Errorf("expected serial log file to contain marker %q, got:\n%s", marker, contents)
	}
	return nil
}

// RunShouldNotBeCached verifies two Run calls on one Machine re-execute QEMU
// rather than cache-hitting: each boot emits fresh /dev/urandom bytes, so the
// two serial consoles must differ.
//
// +cache="never"
func (t *Tests) RunShouldNotBeCached(ctx context.Context) error {
	m, marker, err := firmwareMachine(ctx, "nocache")
	if err != nil {
		return err
	}
	out1, err := m.Run(ctx, dagger.QemuMachineRunOpts{TimeoutSeconds: 180})
	if err != nil {
		return err
	}
	out2, err := m.Run(ctx, dagger.QemuMachineRunOpts{TimeoutSeconds: 180})
	if err != nil {
		return err
	}
	if !strings.Contains(out1, marker) || !strings.Contains(out2, marker) {
		return fmt.Errorf("expected both runs to boot and print marker %q", marker)
	}
	if out1 == out2 {
		return fmt.Errorf("expected two Run invocations to differ (uncached), but the serial consoles were identical")
	}
	return nil
}

// -----------------------------------------------------------------------------
// Boot (real Alpine aarch64 kernel) tests
// -----------------------------------------------------------------------------

// DefaultsBootArm64Kernel verifies the documented defaults boot a working VM:
// arch=AARCH64 with empty machine/cpu must resolve to virt + cortex-a53 and
// reach userspace.
//
// +cache="never"
func (t *Tests) DefaultsBootArm64Kernel(ctx context.Context) error {
	tok, err := randName(ctx, "")
	if err != nil {
		return err
	}
	marker := "QEMUDEFAULTS" + tok
	m := dag.Qemu().Linux(alpineArm64Kernel(), dagger.QemuLinuxOpts{
		Initrd:  firmwareInitramfs(marker),
		Arch:    dagger.QemuArchAarch64, // machine/cpu left empty => virt + cortex-a53
		Cmdline: "console=ttyAMA0 rdinit=/init",
		Name:    "qemu-defaults-" + tok,
	})
	out, err := m.Run(ctx, dagger.QemuMachineRunOpts{TimeoutSeconds: 180})
	if err != nil {
		return err
	}
	if !strings.Contains(out, marker) {
		return fmt.Errorf("expected default-configured arm64 VM to boot and print marker %q, got:\n%s", marker, out)
	}
	return nil
}

// LinuxBootReachesUserspace verifies a real Alpine aarch64 kernel hands off to
// PID 1 in userspace: the kernel logs the /init handoff and userspace then runs
// a working `uname` syscall.
//
// +cache="never"
func (t *Tests) LinuxBootReachesUserspace(ctx context.Context) error {
	m, marker, err := firmwareMachine(ctx, "userspace")
	if err != nil {
		return err
	}
	out, err := m.Run(ctx, dagger.QemuMachineRunOpts{TimeoutSeconds: 180})
	if err != nil {
		return err
	}
	if !strings.Contains(out, "Run /init as init process") {
		return fmt.Errorf("expected kernel to hand off to userspace (/init), got:\n%s", out)
	}
	if !strings.Contains(out, "USERSPACE_OK") || !strings.Contains(out, marker) {
		return fmt.Errorf("expected userspace to run (USERSPACE_OK + marker %q), got:\n%s", marker, out)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Networking (service-bind end-to-end) tests
// -----------------------------------------------------------------------------

// ServiceForwardedPortReachable boots a guest that brings up networking and
// listens on a runtime-minted port, forwards it over slirp hostfwd, binds the
// machine into a fresh consumer container, and asserts the port is reachable —
// proving hostfwd + slirp + WithServiceBinding wiring end-to-end.
//
// +cache="never"
func (t *Tests) ServiceForwardedPortReachable(ctx context.Context) error {
	port, err := randPort(ctx)
	if err != nil {
		return err
	}
	m, err := serviceMachine(ctx, "reach", port)
	if err != nil {
		return err
	}
	defer m.Stop(ctx)
	// Read the guest's per-boot token (not a bare connect): a slirp hostfwd port
	// accepts host-side even before the guest listens, so only an actual data
	// exchange proves the listener is reachable end-to-end.
	tok, err := reachToken(ctx, m, port, 60)
	if err != nil {
		return fmt.Errorf("forwarded port %d was not reachable through the bound machine: %w", port, err)
	}
	if tok == "" {
		return fmt.Errorf("reached port %d but read an empty identity token", port)
	}
	return nil
}

// StopHaltsService proves Stop's +cache="never" teardown actually halts the
// backing service (the postgres EndpointShouldNotBeCached lifecycle analog).
// The guest serves a per-boot identity token. We pin the service up with an
// explicit Start so it stays one instance across reads, read the token, Stop
// the machine, then read again — the second read re-binds and, because Stop
// killed the original VM, gets a *fresh* boot with a different token. A no-op
// Stop would leave the pinned instance serving the same token, so an unchanged
// token fails the test. This is robust to fast TCG boots (it relies on the
// restart, not on out-racing it); the per-read nonce keeps the two reads from
// cache-colliding.
//
// +cache="never"
func (t *Tests) StopHaltsService(ctx context.Context) error {
	port, err := randPort(ctx)
	if err != nil {
		return err
	}
	m, err := serviceMachine(ctx, "stop", port)
	if err != nil {
		return err
	}
	// Pin the backing service up so it stays a single instance across the first
	// and second reads absent a teardown; defer cleanup.
	if _, err := m.Service().Start(ctx); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	defer m.Stop(ctx)

	tok1, err := reachToken(ctx, m, port, 60)
	if err != nil {
		return fmt.Errorf("service never became reachable on port %d before Stop: %w", port, err)
	}
	if tok1 == "" {
		return fmt.Errorf("first read returned an empty identity token")
	}
	if err := m.Stop(ctx); err != nil {
		return fmt.Errorf("stop machine: %w", err)
	}
	tok2, err := reachToken(ctx, m, port, 60)
	if err != nil {
		return fmt.Errorf("service did not come back after Stop+rebind: %w", err)
	}
	if tok2 == "" {
		return fmt.Errorf("second read returned an empty identity token")
	}
	if tok1 == tok2 {
		return fmt.Errorf("expected Stop to tear down the VM (fresh boot => new token), but the identity token was unchanged: %q", tok1)
	}
	return nil
}
