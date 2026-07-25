package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"dagger/valkey/internal/dagger"
)

// Fixed in-container paths for the caller-supplied configuration
// passthrough. The config file is an ordinary world-readable file; the
// ACL file rides in as a mounted secret because it carries per-user
// password hashes (and, for a caller who writes one carelessly,
// plaintext). Both are owned by the valkey user the entrypoint drops to,
// so valkey-server can still read them after `gosu valkey`.
const (
	serverConfigPath  = "/etc/valkey/valkey.conf"
	serverAclFilePath = "/etc/valkey/users.acl"
)

// defaultMaxMemoryPolicy is both Valkey's own default eviction policy and
// this module's `+default` for maxMemoryPolicy. Because the two agree, a
// caller who never touches the parameter gets no `--maxmemory-policy`
// flag at all — see applyServerConfig for why that matters.
const defaultMaxMemoryPolicy = "noeviction"

// serverConfig carries the `valkey-server` configuration a caller wants
// applied at boot. Every field is optional; the zero value renders no
// mounts and no arguments, so a node built from it is byte-identical to
// one built before this passthrough existed.
//
// These are boot-time settings rather than post-boot modifiers because
// several of them (an ACL file, an append-only log, a config file) are
// only read while valkey-server starts up.
type serverConfig struct {
	File            *dagger.File   // A valkey.conf loaded ahead of every flag argument.
	AclFile         *dagger.Secret // An ACL file mounted as a secret and loaded via --aclfile.
	AppendOnly      bool           // Turn the AOF on (`--appendonly yes`).
	MaxMemory       string         // Memory ceiling in Valkey's own notation ("512mb", "1gb", "104857600").
	MaxMemoryPolicy string         // Eviction policy applied once MaxMemory is reached.
	ExtraArgs       []string       // Unvalidated escape hatch, appended after everything else.
}

// maxMemoryPolicies is the set of eviction policies Valkey accepts. A
// typo here (`allkeys-lfu` mistyped as `allkeys-flu`, say) makes
// valkey-server refuse to start with a config-parse error buried in the
// service's logs, long after the Dagger call that caused it — so the set
// is checked in-process instead.
var maxMemoryPolicies = []string{
	"noeviction",
	"allkeys-lru",
	"allkeys-lfu",
	"allkeys-random",
	"volatile-lru",
	"volatile-lfu",
	"volatile-random",
	"volatile-ttl",
}

// maxMemoryPattern matches what Valkey's own memtoll parser accepts: an
// integer optionally followed by a unit suffix, case-insensitively. The
// suffixes are deliberately NOT interchangeable — `k`/`m`/`g` are powers
// of 1000 and `kb`/`mb`/`gb` powers of 1024 — so the value is passed
// through verbatim rather than normalised. A fractional value ("1.5gb")
// is rejected because memtoll stops at the `.` and fails on the
// remainder.
var maxMemoryPattern = regexp.MustCompile(`^[0-9]+([bB]|[kK][bB]?|[mM][bB]?|[gG][bB]?)?$`)

// validateServerConfig rejects a passthrough that would make
// valkey-server die during startup. Both checks catch a class of failure
// that is otherwise near-invisible: the node's service simply never
// becomes ready, and the caller sees a readiness timeout rather than the
// typo that caused it.
//
// The config file, the ACL file, and ExtraArgs are deliberately NOT
// validated — the first two are opaque to this module and the third is
// documented as unsupported surface.
func validateServerConfig(cfg *serverConfig) error {
	if cfg.MaxMemory != "" && !maxMemoryPattern.MatchString(cfg.MaxMemory) {
		return fmt.Errorf(
			"maxMemory %q is not a valid Valkey memory value: expected an integer with an optional unit suffix (b, k, kb, m, mb, g, gb), e.g. %q or %q",
			cfg.MaxMemory, "512mb", "104857600",
		)
	}
	if cfg.MaxMemoryPolicy != "" && !isMaxMemoryPolicy(cfg.MaxMemoryPolicy) {
		return fmt.Errorf(
			"maxMemoryPolicy %q is not a valid Valkey eviction policy: expected one of %s",
			cfg.MaxMemoryPolicy, strings.Join(maxMemoryPolicies, ", "),
		)
	}
	return nil
}

// validateAclFile rejects an ACL file that never mentions the `default`
// user.
//
// This is not pedantry, it is the module's password guarantee. Valkey
// applies `requirepass` while parsing the config and loads the ACL file
// afterwards, and an ACL file that omits `default` does not leave that
// user alone — it recreates it in its factory state, which is `on
// nopass ~* +@all`. So an ACL file listing only the caller's own users
// silently un-does `requirepass` and leaves the node reachable by
// anything that can route to it. Valkey.Server rejects `password == nil`
// for exactly that reason; letting `aclFile` reintroduce it through the
// back door would make the guarantee a lie.
//
// The check is deliberately a mention, not an interpretation: `user
// default off` and `user default on >… ~… +…` are both fine, because a
// caller who named `default` has made a decision about it. Only silence
// is refused.
//
// Reading the secret here costs nothing extra in exposure — the same
// bytes are mounted into the node a moment later — and nothing is logged.
func validateAclFile(ctx context.Context, aclFile *dagger.Secret) error {
	contents, err := aclFile.Plaintext(ctx)
	if err != nil {
		return fmt.Errorf("read aclFile: %w", err)
	}
	for _, line := range strings.Split(contents, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if strings.EqualFold(fields[0], "user") && fields[1] == defaultUser {
			return nil
		}
	}
	return fmt.Errorf(
		"aclFile must contain a `user %s ...` rule: Valkey loads the ACL file after requirepass and recreates any user the file omits in its factory state (`on nopass`), so an ACL file that lists only your own users would silently drop the password from the %s user and leave the node open; add `user %s on >$password ~* &* +@all` to keep the requirepass credentials, or `user %s off` to disable it deliberately",
		defaultUser, defaultUser, defaultUser, defaultUser,
	)
}

// isMaxMemoryPolicy reports whether policy is one Valkey accepts.
func isMaxMemoryPolicy(policy string) bool {
	for _, known := range maxMemoryPolicies {
		if policy == known {
			return true
		}
	}
	return false
}

// applyServerConfig renders the container mutations and the
// `valkey-server` arguments that realise a configuration passthrough. It
// returns the (possibly mutated) container plus TWO argument slices,
// because they belong at opposite ends of the command line:
//
//   - leading — the config file path, which valkey-server only honours as
//     its FIRST positional argument. Everything after it is a flag
//     override.
//   - flags — the individual settings, which must therefore follow every
//     directive the file supplied in order to win over it.
//
// The rule the flags follow: a parameter left at its default emits NO
// flag. That is what keeps `configFile` meaningful. A flag beats the
// file, so unconditionally emitting `--appendonly no` (Valkey's default,
// and this module's) would silently override a config file that turned
// the AOF on, and the caller would have no way to express "leave it to
// the file". Emitting nothing until the caller actually asks for
// something means the file's directive survives untouched.
//
// ExtraArgs is not rendered here: buildServer appends it after the
// topology arguments so it is genuinely last on the command line.
func applyServerConfig(ctr *dagger.Container, cfg *serverConfig) (*dagger.Container, []string, []string) {
	var leading, flags []string

	if cfg.File != nil {
		ctr = ctr.WithFile(serverConfigPath, cfg.File, dagger.ContainerWithFileOpts{
			Permissions: 0o644,
			Owner:       "valkey:valkey",
		})
		leading = append(leading, serverConfigPath)
	}

	if cfg.AclFile != nil {
		ctr = ctr.WithMountedSecret(serverAclFilePath, cfg.AclFile, dagger.ContainerWithMountedSecretOpts{
			Mode:  0o600,
			Owner: "valkey:valkey",
		})
		flags = append(flags, "--aclfile", serverAclFilePath)
	}

	if cfg.AppendOnly {
		flags = append(flags, "--appendonly", "yes")
	}
	if cfg.MaxMemory != "" {
		flags = append(flags, "--maxmemory", cfg.MaxMemory)
	}
	if cfg.MaxMemoryPolicy != "" && cfg.MaxMemoryPolicy != defaultMaxMemoryPolicy {
		flags = append(flags, "--maxmemory-policy", cfg.MaxMemoryPolicy)
	}

	return ctr, leading, flags
}
