package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"dagger/qemu/internal/dagger"
)

// --- Drive mode A: service-bind (long-running OS images) ---

// Host returns the per-machine hostname the backing service is reachable at.
//
// +cache="never"
func (m *Machine) Host() string {
	return m.Hostname
}

// Endpoint returns `host:port` for a forwarded TCP port, erroring if the port
// was not in the machine's `tcpPorts`.
//
// +cache="never"
func (m *Machine) Endpoint(port int) (string, error) {
	for _, p := range m.Ports {
		if p == port {
			return fmt.Sprintf("%s:%d", m.Hostname, port), nil
		}
	}
	return "", fmt.Errorf("port %d is not forwarded by this machine (forwarded ports: %v)", port, m.Ports)
}

// Service returns the long-running guest as a *dagger.Service. Consumers can
// bind it (see Bind) and reach forwarded ports at Endpoint.
//
// +cache="never"
func (m *Machine) Service() *dagger.Service {
	return m.Svc
}

// Bind attaches the guest service to ctr under the machine's hostname so ctr
// can dial the forwarded ports at `Host()`.
//
// +cache="never"
func (m *Machine) Bind(ctr *dagger.Container) *dagger.Container {
	return ctr.WithServiceBinding(m.Hostname, m.Svc)
}

// Stop tears down the backing service. Tests should defer this so the service
// span closes when the test returns. SIGKILL skips graceful shutdown.
//
// +cache="never"
func (m *Machine) Stop(ctx context.Context) error {
	if m.Svc == nil {
		return nil
	}
	if _, err := m.Svc.Stop(ctx, dagger.ServiceStopOpts{Kill: true}); err != nil {
		return fmt.Errorf("stop qemu machine: %w", err)
	}
	return nil
}

// --- Drive mode B: run-to-completion (firmware serial capture) ---

// Run boots the guest to completion (`-no-reboot`) and returns the captured
// serial console. A guest that powers off exits before `timeoutSeconds`; one
// that doesn't is killed at the deadline and whatever serial it produced is
// returned.
//
// +cache="never"
func (m *Machine) Run(
	ctx context.Context,
	// +default=300
	timeoutSeconds int,
) (string, error) {
	return m.runSerial(ctx, timeoutSeconds)
}

// WaitForLine runs the guest and returns the serial console only if it
// contains substr, erroring otherwise.
//
// +cache="never"
func (m *Machine) WaitForLine(
	ctx context.Context,
	substr string,
	// +default=300
	timeoutSeconds int,
) (string, error) {
	out, err := m.runSerial(ctx, timeoutSeconds)
	if err != nil {
		return "", err
	}
	if !strings.Contains(out, substr) {
		return "", fmt.Errorf("serial console did not contain %q within %ds:\n%s", substr, timeoutSeconds, out)
	}
	return out, nil
}

// SerialLog runs the guest and materializes the captured serial console as a
// *dagger.File via the module workdir — no helper container.
//
// +cache="never"
func (m *Machine) SerialLog(
	ctx context.Context,
	// +default=300
	timeoutSeconds int,
) (*dagger.File, error) {
	out, err := m.runSerial(ctx, timeoutSeconds)
	if err != nil {
		return nil, err
	}
	return writeWorkdirFile("serial.log", []byte(out))
}

// RunResult pairs the serial console output of a finite boot with the guest's
// exit code. On the bare-metal path the exit code is the semihosting SYS_EXIT
// value (0 = success); see Machine.RunStatus.
type RunResult struct {
	// Output is the captured serial console output.
	Output string
	// ExitCode is the guest exit code (semihosting SYS_EXIT; 0 = success). A
	// guest that doesn't power off before the deadline is killed and yields the
	// timeout code (124) — see runSerialStatus for why it isn't the raw SIGKILL
	// code (137).
	ExitCode int
}

// RunStatus boots the guest to completion like Run, but returns both the
// captured serial console and the guest exit code in a single boot — the
// bare-metal counterpart to Run, where the semihosting SYS_EXIT code carries
// pass/fail that serial text alone can't. Run / WaitForLine / SerialLog are
// unchanged and still return serial only.
//
// +cache="never"
func (m *Machine) RunStatus(
	ctx context.Context,
	// +default=300
	timeoutSeconds int,
) (*RunResult, error) {
	out, code, err := m.runSerialStatus(ctx, timeoutSeconds)
	if err != nil {
		return nil, err
	}
	return &RunResult{Output: out, ExitCode: code}, nil
}

// runSerial executes one finite QEMU boot and returns its serial console,
// discarding the exit code — the serial-only path behind Run / WaitForLine /
// SerialLog.
func (m *Machine) runSerial(ctx context.Context, timeoutSeconds int) (string, error) {
	out, _, err := m.runSerialStatus(ctx, timeoutSeconds)
	return out, err
}

// runSerialStatus executes one finite QEMU boot and returns both its serial
// console and its exit code, read from the same exec.
//
// A per-call nonce env var busts Dagger's layer cache so each invocation
// re-runs QEMU (without it, two identical Run calls would cache-hit and
// return the same console — the `+cache="never"` on the function controls the
// function result, not the inner WithExec). `timeout -s KILL N` bounds a guest
// that never powers off, and Expect=Any tolerates the resulting non-zero exit
// (a semihosting SYS_EXIT failure code, or the timeout) so the captured serial
// and the exit code are both still returned.
//
// The timeout runs inside a tiny `sh -c` wrapper that rewrites the SIGKILL
// exit (137) to the conventional timeout code (124). Expect=Any only tolerates
// exit codes 0-127 and 192-255 (see dagger.ReturnTypeAny): a signal-kill exit
// in 128-191 is treated as an exec error, which would make Stdout fail and
// discard the captured serial. Remapping 137 -> 124 keeps a timed-out boot on
// the value path — its serial is returned alongside a non-zero exit code.
func (m *Machine) runSerialStatus(ctx context.Context, timeoutSeconds int) (string, int, error) {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 300
	}
	nonce, err := randHex()
	if err != nil {
		return "", 0, err
	}
	// $0 is the timeout in seconds; "$@" is the QEMU argv. 137 = 128 + SIGKILL.
	const wrap = `timeout -s KILL "$0" "$@"; ec=$?; [ "$ec" -eq 137 ] && ec=124; exit "$ec"`
	args := append([]string{"sh", "-c", wrap, strconv.Itoa(timeoutSeconds)}, m.RunArgv...)
	ran := m.Base.
		WithEnvVariable("QEMU_RUN_NONCE", nonce).
		WithExec(args, dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny})
	out, err := ran.Stdout(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("run qemu: %w", err)
	}
	code, err := ran.ExitCode(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("run qemu: read exit code: %w", err)
	}
	return out, code, nil
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

// writeWorkdirFile writes content to a content-addressed subdir of the
// module's scratch workdir and returns it as a *dagger.File. The subdir name
// is a hash of the content, so distinct outputs land at distinct paths and
// identical outputs are idempotent. Pure-Go; no helper container.
func writeWorkdirFile(name string, content []byte) (*dagger.File, error) {
	sum := sha256.Sum256(content)
	dir := "out-" + hex.EncodeToString(sum[:])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)

	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return nil, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return nil, err
	}
	return dag.CurrentModule().WorkdirFile(path), nil
}
