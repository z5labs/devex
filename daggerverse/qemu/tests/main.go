// Tests for the qemu daggerverse module. Each test is exposed as a standalone
// dagger function so it can be invoked individually during TDD; All wires them
// up for parallel execution under `dagger call all`. The three sub-aggregators
// (Validation, Firmware, Boot) each carry `+check` so CI schedules them onto
// their own runners.
package main

import (
	"context"
	"fmt"
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
