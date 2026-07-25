package main

// The state-manipulation half of the tofu CLI: what an operator reaches for
// once state and reality have diverged. The lifecycle functions in config.go
// are what a pipeline runs unattended; these are what someone runs on purpose.
//
// Every one of them writes state, so every one hands the resulting
// terraform.tfstate back the way Apply and Destroy do — through runMutation,
// which emits it only in file-carried mode and forfeits it on a non-zero exit.
// Graph is the exception: it reads the configuration and returns text.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"dagger/opentofu/internal/dagger"
)

const (
	// noStateMarker is how tofu announces that the state commands found no
	// state file at all. An emptied state file is not this case: tofu lists it
	// as zero resources and exits 0. Only a wholly absent one is refused, and
	// StateList answers that with the same empty listing rather than an error.
	noStateMarker = "No state file was found"

	// uiLevelKey marks a line of tofu's -json output as a UI message rather
	// than the document being asked for. Every -json subcommand prefixes its
	// payload with at least a version message, so the stream has to be sifted.
	uiLevelKey = "@level"
)

// ---------------------------------------------------------------- inspection

// StateList returns the resource addresses in state, one per line
// (`tofu state list`).
//
// A configuration with no state yet lists nothing rather than failing: tofu
// itself refuses a wholly absent state file, but "no state" and "an emptied
// state" hold the same answer to what is under management, and a listing that
// distinguishes them only makes the caller handle a case with no content.
//
// +cache="never"
func (c *Config) StateList(ctx context.Context) (string, error) {
	args := []string{"tofu", "state", "list", "-no-color"}
	args = append(args, c.varArgs()...)

	exec, err := c.readExec(ctx, args, true)
	if err != nil {
		return "", err
	}
	code, err := exec.ExitCode(ctx)
	if err != nil {
		return "", err
	}
	if code != 0 {
		out := combinedOutput(ctx, exec)
		if strings.Contains(out, noStateMarker) {
			return "", nil
		}
		return "", fmt.Errorf("tofu state list failed (exit %d):\n%s", code, out)
	}
	out, err := exec.Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("tofu state list: %s", errText(err))
	}
	return out, nil
}

// StateShow returns the state of one resource as JSON
// (`tofu state show -json <address>`).
//
// An address that matches nothing in state is an error naming it, rather than
// an empty document a caller could mistake for a resource with no attributes.
//
// Note what the JSON form does not do: `-json` prints sensitive values in
// full, whether or not the variable behind one was declared
// `sensitive = true`. Treat the result as sensitive whenever the resource is.
//
// +cache="never"
func (c *Config) StateShow(
	ctx context.Context,
	// Address of the resource instance to show, e.g. `random_pet.name`.
	address string,
) (string, error) {
	if err := checkPositional("StateShow", "resource address", address); err != nil {
		return "", err
	}
	args := []string{"tofu", "state", "show", "-json"}
	args = append(args, c.varArgs()...)
	args = append(args, address)

	stream, err := c.read(ctx, args, "tofu state show "+address, true)
	if err != nil {
		return "", err
	}
	return jsonDocument(stream, "tofu state show -json "+address)
}

// Graph returns the DOT rendering of the configuration's dependency graph
// (`tofu graph`), for GraphViz or anything else that reads the format.
//
// It is the one member of this file that is read-only, and it is derived from
// the configuration rather than from live state — so unlike the rest it caches
// for the session.
//
// +cache="session"
func (c *Config) Graph(ctx context.Context) (string, error) {
	args := []string{"tofu", "graph", "-no-color"}
	args = append(args, c.varArgs()...)
	return c.read(ctx, args, "tofu graph", false)
}

// ------------------------------------------------------------------ surgery

// StateMv renames a resource in state (`tofu state mv <from> <to>`) and
// returns the resulting state directory.
//
// This is how a resource survives being renamed in the configuration: without
// it, tofu reads the new name as a new resource and the old one as gone, and
// plans to destroy and recreate.
//
// +cache="never"
func (c *Config) StateMv(
	ctx context.Context,
	// Address the resource is recorded under now.
	from string,
	// Address to record it under instead.
	to string,
) (*dagger.Directory, error) {
	if err := checkPositional("StateMv", "source address", from); err != nil {
		return nil, err
	}
	if err := checkPositional("StateMv", "destination address", to); err != nil {
		return nil, err
	}
	ctr, err := c.initialized(ctx)
	if err != nil {
		return nil, err
	}
	args := []string{"tofu", "state", "mv", "-no-color"}
	args = append(args, c.varArgs()...)
	args = append(args, from, to)
	return c.runMutation(ctx, ctr, args, fmt.Sprintf("tofu state mv %s %s", from, to), "state-mv.log")
}

// StateRm drops resources from state (`tofu state rm <address>...`) and
// returns the resulting state directory.
//
// The objects themselves are left alone: this is how a resource is handed over
// to another configuration, or abandoned to be managed by hand. A plan run
// afterwards sees them as absent and proposes creating them again, which is
// the hazard — the objects are still out there.
//
// +cache="never"
func (c *Config) StateRm(
	ctx context.Context,
	// Addresses to drop from state.
	addresses []string,
) (*dagger.Directory, error) {
	if len(addresses) == 0 {
		return nil, fmt.Errorf("StateRm: at least one resource address is required")
	}
	for _, address := range addresses {
		if err := checkPositional("StateRm", "resource address", address); err != nil {
			return nil, err
		}
	}
	ctr, err := c.initialized(ctx)
	if err != nil {
		return nil, err
	}
	args := []string{"tofu", "state", "rm", "-no-color"}
	args = append(args, c.varArgs()...)
	args = append(args, addresses...)
	return c.runMutation(ctx, ctr, args, "tofu state rm "+strings.Join(addresses, " "), "state-rm.log")
}

// Import brings an existing object under management
// (`tofu import <address> <id>`) and returns the resulting state directory.
//
// The address has to already be declared in the configuration — import writes
// state, it does not write HCL — and the id is whatever the resource's own
// provider accepts, which varies per resource type.
//
// +cache="never"
func (c *Config) Import(
	ctx context.Context,
	// Address the object is to be recorded under, as declared in the
	// configuration.
	address string,
	// Provider-specific id of the existing object.
	id string,
) (*dagger.Directory, error) {
	if err := checkPositional("Import", "resource address", address); err != nil {
		return nil, err
	}
	if err := checkPositional("Import", "resource id", id); err != nil {
		return nil, err
	}
	ctr, err := c.initialized(ctx)
	if err != nil {
		return nil, err
	}
	args := []string{"tofu", "import", "-input=false", "-no-color"}
	args = append(args, c.varArgs()...)
	args = append(args, address, id)
	return c.runMutation(ctx, ctr, args, "tofu import "+address, "import.log")
}

// Refresh updates state to match what the providers report
// (`tofu apply -refresh-only -auto-approve`) and returns the resulting state
// directory.
//
// It is the refresh-only apply rather than the deprecated `tofu refresh`:
// both write state, but only this one is still supported.
//
// +cache="never"
func (c *Config) Refresh(ctx context.Context) (*dagger.Directory, error) {
	ctr, err := c.initialized(ctx)
	if err != nil {
		return nil, err
	}
	args := []string{"tofu", "apply", "-refresh-only", "-auto-approve", "-input=false", "-no-color"}
	args = append(args, c.varArgs()...)
	return c.runMutation(ctx, ctr, args, "tofu apply -refresh-only", "refresh.log")
}

// Taint marks a resource for replacement (`tofu taint <address>`) and returns
// the resulting state directory. The next plan proposes destroying and
// recreating it.
//
// +cache="never"
func (c *Config) Taint(
	ctx context.Context,
	// Address of the resource instance to mark.
	address string,
) (*dagger.Directory, error) {
	return c.mark(ctx, "Taint", "taint", address)
}

// Untaint clears the mark Taint set (`tofu untaint <address>`) and returns the
// resulting state directory.
//
// +cache="never"
func (c *Config) Untaint(
	ctx context.Context,
	// Address of the resource instance to clear.
	address string,
) (*dagger.Directory, error) {
	return c.mark(ctx, "Untaint", "untaint", address)
}

// ---------------------------------------------------------------- internals

// mark runs taint or untaint, which differ only in the subcommand name.
func (c *Config) mark(ctx context.Context, fn string, subcommand string, address string) (*dagger.Directory, error) {
	if err := checkPositional(fn, "resource address", address); err != nil {
		return nil, err
	}
	ctr, err := c.initialized(ctx)
	if err != nil {
		return nil, err
	}
	args := []string{"tofu", subcommand, "-no-color"}
	args = append(args, c.varArgs()...)
	args = append(args, address)
	return c.runMutation(ctx, ctr, args, "tofu "+subcommand+" "+address, subcommand+".log")
}

// jsonDocument pulls the payload out of a tofu `-json` output.
//
// That output is a JSON *stream*, not a document: tofu prefixes the payload
// with UI messages of its own, starting with the version it ran. The UI lines
// carry an `@level` key and the payload does not, which is what separates
// them.
func jsonDocument(stream string, label string) (string, error) {
	for line := range strings.SplitSeq(stream, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			continue
		}
		if _, ui := fields[uiLevelKey]; ui {
			continue
		}
		return line, nil
	}
	return "", fmt.Errorf("%s emitted no JSON document:\n%s", label, stream)
}
