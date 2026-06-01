package main

import (
	"context"
	"fmt"
	"strings"

	par "github.com/dagger/dagger/util/parallel"

	"dagger/tests/internal/dagger"
)

// BareMetal runs the bare-metal firmware + semihosting tests: boot a tiny
// freestanding Cortex-M3 firmware (built reproducibly via the zig module) on
// the MCU-class lm3s6965evb machine and assert that semihosting SYS_WRITE0
// reaches the serial console and SYS_EXIT surfaces as a guest exit code. These
// are fast TCG boots (the firmware writes a marker and exits immediately).
//
// +check
// +cache="session"
func (t *Tests) BareMetal(
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
	jobs = jobs.WithJob("bare-metal-boots-and-captures-serial", t.BareMetalBootsAndCapturesSerial)
	jobs = jobs.WithJob("bare-metal-exit-code-zero-on-pass", t.BareMetalExitCodeZeroOnPass)
	jobs = jobs.WithJob("bare-metal-exit-code-non-zero-on-fail", t.BareMetalExitCodeNonZeroOnFail)
	jobs = jobs.WithJob("bare-metal-rejects-nil-firmware", t.BareMetalRejectsNilFirmware)
	jobs = jobs.WithJob("bare-metal-rejects-unknown-arch", t.BareMetalRejectsUnknownArch)
	return jobs.Run(ctx)
}

// baremetalMachine builds an ARM Cortex-M3 Machine that boots a freestanding
// semihosting firmware which writes marker via SYS_WRITE0 then exits via
// SYS_EXIT(exitCode). label keeps parallel tests on independent session-cached
// machines.
func baremetalMachine(ctx context.Context, label string, marker string, exitCode int) (*dagger.QemuMachine, error) {
	tok, err := randName(ctx, "")
	if err != nil {
		return nil, err
	}
	return dag.Qemu().BareMetal(baremetalFirmware(marker, exitCode), dagger.QemuBareMetalOpts{
		Arch: dagger.QemuArchArm, // machine/cpu left empty => lm3s6965evb + cortex-m3
		Name: "qemu-bm-" + label + "-" + tok,
	}), nil
}

// BareMetalBootsAndCapturesSerial verifies a semihosting firmware's SYS_WRITE0
// marker reaches the serial console through RunStatus, Run, and WaitForLine.
//
// +cache="never"
func (t *Tests) BareMetalBootsAndCapturesSerial(ctx context.Context) error {
	tok, err := randName(ctx, "")
	if err != nil {
		return err
	}
	marker := "QEMUBMMARKER" + tok
	m, err := baremetalMachine(ctx, "serial", marker, 0)
	if err != nil {
		return err
	}
	res := m.RunStatus(dagger.QemuMachineRunStatusOpts{TimeoutSeconds: 120})
	out, err := res.Output(ctx)
	if err != nil {
		return err
	}
	if !strings.Contains(out, marker) {
		return fmt.Errorf("expected RunStatus().Output to contain marker %q, got:\n%s", marker, out)
	}
	// Cross-check the serial-only paths surface the same marker.
	runOut, err := m.Run(ctx, dagger.QemuMachineRunOpts{TimeoutSeconds: 120})
	if err != nil {
		return err
	}
	if !strings.Contains(runOut, marker) {
		return fmt.Errorf("expected Run() output to contain marker %q, got:\n%s", marker, runOut)
	}
	waitOut, err := m.WaitForLine(ctx, marker, dagger.QemuMachineWaitForLineOpts{TimeoutSeconds: 120})
	if err != nil {
		return err
	}
	if !strings.Contains(waitOut, marker) {
		return fmt.Errorf("expected WaitForLine() output to contain marker %q, got:\n%s", marker, waitOut)
	}
	return nil
}

// BareMetalExitCodeZeroOnPass verifies a firmware that calls SYS_EXIT(0) yields
// RunStatus().ExitCode == 0.
//
// +cache="never"
func (t *Tests) BareMetalExitCodeZeroOnPass(ctx context.Context) error {
	tok, err := randName(ctx, "")
	if err != nil {
		return err
	}
	marker := "QEMUBMPASS" + tok
	m, err := baremetalMachine(ctx, "pass", marker, 0)
	if err != nil {
		return err
	}
	res := m.RunStatus(dagger.QemuMachineRunStatusOpts{TimeoutSeconds: 120})
	code, err := res.ExitCode(ctx)
	if err != nil {
		return err
	}
	if code != 0 {
		out, _ := res.Output(ctx)
		return fmt.Errorf("expected SYS_EXIT(0) to yield ExitCode 0, got %d; serial:\n%s", code, out)
	}
	return nil
}

// BareMetalExitCodeNonZeroOnFail verifies a firmware that calls SYS_EXIT with a
// non-zero code yields the corresponding non-zero RunStatus().ExitCode.
//
// +cache="never"
func (t *Tests) BareMetalExitCodeNonZeroOnFail(ctx context.Context) error {
	tok, err := randName(ctx, "")
	if err != nil {
		return err
	}
	const wantCode = 3
	marker := "QEMUBMFAIL" + tok
	m, err := baremetalMachine(ctx, "fail", marker, wantCode)
	if err != nil {
		return err
	}
	res := m.RunStatus(dagger.QemuMachineRunStatusOpts{TimeoutSeconds: 120})
	code, err := res.ExitCode(ctx)
	if err != nil {
		return err
	}
	if code != wantCode {
		out, _ := res.Output(ctx)
		return fmt.Errorf("expected SYS_EXIT(%d) to yield ExitCode %d, got %d; serial:\n%s", wantCode, wantCode, code, out)
	}
	return nil
}

// BareMetalRejectsNilFirmware verifies a nil firmware is rejected. The Dagger
// SDK binding panics via assertNotNil before the call leaves the test module;
// recover and assert the panic mentions the rejected argument.
//
// +cache="never"
func (t *Tests) BareMetalRejectsNilFirmware(ctx context.Context) (returnErr error) {
	defer func() {
		r := recover()
		if r == nil {
			returnErr = fmt.Errorf("expected BareMetal(nil) to panic via assertNotNil, but it did not")
			return
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "firmware") {
			returnErr = fmt.Errorf("expected panic to mention firmware, got: %v", r)
		}
	}()
	m := dag.Qemu().BareMetal(nil)
	_, _ = m.Host(ctx)
	return nil
}

// BareMetalRejectsUnknownArch verifies BareMetal errors for an arch with no
// bare-metal (MCU) profile — e.g. a SoC arch like X86_64 that Linux/Disk
// accept but BareMetal does not.
//
// +cache="never"
func (t *Tests) BareMetalRejectsUnknownArch(ctx context.Context) error {
	m := dag.Qemu().BareMetal(placeholderFile("firmware"), dagger.QemuBareMetalOpts{
		Arch: dagger.QemuArchX8664,
	})
	if _, err := m.Host(ctx); err == nil {
		return fmt.Errorf("expected BareMetal(arch=X86_64) to fail with an unsupported-arch error, got nil error")
	} else if !strings.Contains(err.Error(), "bare-metal arch") {
		return fmt.Errorf("expected error to mention 'bare-metal arch', got: %v", err)
	}
	return nil
}
