package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/tests/internal/dagger"
)

// PushImageUntaggedPublishesNoTag asserts that an untagged push puts the
// bytes in the registry and leaves nothing that names them.
//
// Both halves matter and they fail in opposite directions. A push that landed
// no manifest would make the untagged mode useless — there would be nothing to
// tag afterwards — and a push that landed a tag would make it pointless, since
// the whole reason a caller reaches for it is to do fallible work before
// anything a consumer resolves can reach the image. So the manifest is read
// back by digest, and the tag listing is read back and has to be empty.
func (t *Tests) PushImageUntaggedPublishesNoTag(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "untagged")
	if err != nil {
		return err
	}

	variants := []*dagger.Container{baseImage("linux/amd64"), baseImage("linux/arm64")}
	digest, err := reg.client().PushImageUntagged(ctx, repo, variants)
	if err != nil {
		return fmt.Errorf("PushImageUntagged: %v", err)
	}
	if !strings.HasPrefix(digest, "sha256:") {
		return fmt.Errorf("PushImageUntagged returned %q, want a sha256 digest", digest)
	}

	// Read back by digest: the manifest list is there, naming every variant,
	// exactly as a tagged push would have left it.
	raw, err := reg.client().Manifest(ctx, repo, digest)
	if err != nil {
		return fmt.Errorf("Manifest by digest after an untagged push: %v", err)
	}
	platforms, err := indexPlatforms(raw)
	if err != nil {
		return err
	}
	if err := wantPlatforms(platforms, raw, "linux/amd64", "linux/arm64"); err != nil {
		return err
	}

	tags, err := reg.tags(ctx, repo)
	if err != nil {
		return err
	}
	if len(tags) != 0 {
		return fmt.Errorf("an untagged push left the tags %v in %s, want none", tags, repo)
	}
	return nil
}

// TagNamesAnUntaggedDigestAndMovesAnExistingTag asserts the second half of the
// split: a digest becomes resolvable when, and only when, Tag says so, and a
// tag already pointing somewhere is moved rather than refused.
//
// Moving is in the same test as naming because they are one operation at the
// registry — a manifest PUT under a name — and a Tag that could only create
// would be discovered by the first caller re-publishing a version.
func (t *Tests) TagNamesAnUntaggedDigestAndMovesAnExistingTag(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "tagging")
	if err != nil {
		return err
	}

	// Two images whose bytes differ, so the tag has somewhere to move to and
	// the move is visible as a digest change rather than as a no-op.
	firstMarker, err := uniqueName(ctx, "first")
	if err != nil {
		return err
	}
	secondMarker, err := uniqueName(ctx, "second")
	if err != nil {
		return err
	}
	first, err := reg.client().PushImageUntagged(ctx, repo,
		[]*dagger.Container{baseImage("linux/amd64").WithNewFile("/marker", firstMarker)})
	if err != nil {
		return fmt.Errorf("PushImageUntagged (first): %v", err)
	}
	second, err := reg.client().PushImageUntagged(ctx, repo,
		[]*dagger.Container{baseImage("linux/amd64").WithNewFile("/marker", secondMarker)})
	if err != nil {
		return fmt.Errorf("PushImageUntagged (second): %v", err)
	}
	if first == second {
		return fmt.Errorf("both pushes produced %s, so this test cannot tell a moved tag from a stationary one", first)
	}

	// Nothing resolves the tag until Tag is called, which is the guarantee the
	// publish path is built on.
	if _, err := reg.client().Resolve(ctx, repo, "v1"); err == nil {
		return fmt.Errorf("v1 resolved in %s before anything tagged it", repo)
	}

	tagged, err := reg.client().Tag(ctx, repo, first, "v1")
	if err != nil {
		return fmt.Errorf("Tag (first): %v", err)
	}
	if tagged != first {
		return fmt.Errorf("Tag reported %s, want the digest it was given, %s", tagged, first)
	}
	resolved, err := reg.client().Resolve(ctx, repo, "v1")
	if err != nil {
		return fmt.Errorf("Resolve after Tag: %v", err)
	}
	if resolved != first {
		return fmt.Errorf("v1 resolves to %s, want %s", resolved, first)
	}

	if _, err := reg.client().Tag(ctx, repo, second, "v1"); err != nil {
		return fmt.Errorf("Tag (second, onto an existing tag): %v", err)
	}
	resolved, err = reg.client().Resolve(ctx, repo, "v1")
	if err != nil {
		return fmt.Errorf("Resolve after the second Tag: %v", err)
	}
	if resolved != second {
		return fmt.Errorf("v1 resolves to %s after being moved, want %s", resolved, second)
	}

	// One tag, not two: tagging is naming, not copying.
	tags, err := reg.tags(ctx, repo)
	if err != nil {
		return err
	}
	if len(tags) != 1 || tags[0] != "v1" {
		return fmt.Errorf("tagging twice left %v, want exactly [v1]", tags)
	}
	return nil
}

// TagFailsForAnAbsentDigest asserts Tag refuses to name bytes that are not
// there, and leaves no tag behind when it does.
//
// A registry will accept a manifest PUT under any name it is handed, so a Tag
// that trusted its digest argument could create a tag resolving to a manifest
// whose blobs were never uploaded — an image that exists until somebody pulls
// it. Reading the digest first is what makes the failure happen before the
// name exists rather than after.
func (t *Tests) TagFailsForAnAbsentDigest(ctx context.Context) error {
	reg, err := newRegistry(ctx)
	if err != nil {
		return err
	}
	repo, err := uniqueName(ctx, "absent-subject")
	if err != nil {
		return err
	}
	// A real image, so the repository exists and the failure is about the
	// digest rather than about the repository.
	if _, err := reg.client().PushImage(ctx, repo, "real", []*dagger.Container{baseImage("linux/amd64")}); err != nil {
		return fmt.Errorf("PushImage: %v", err)
	}

	hex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return fmt.Errorf("random sha256 (absent digest): %v", err)
	}
	absent := "sha256:" + hex

	_, err = reg.client().Tag(ctx, repo, absent, "v1")
	if err == nil {
		return fmt.Errorf("Tag(%s, %s) succeeded for a digest the registry never received", repo, absent)
	}
	if !strings.Contains(err.Error(), absent) {
		return fmt.Errorf("the Tag failure does not name the digest %s: %v", absent, err)
	}
	if _, err := reg.client().Resolve(ctx, repo, "v1"); err == nil {
		return fmt.Errorf("a failed Tag left v1 resolvable in %s", repo)
	}
	return nil
}

// TagRefusesIncompleteArguments asserts Tag names the argument that is missing
// rather than sending an unusable reference at a registry.
//
// It needs no registry, and that is not an economy — it is the assertion. Tag
// validates before it connects, so a case that reached a registry at all would
// mean the validation had been skipped. The handle below names a host nothing
// resolves and has no service behind it, so anything that got as far as the
// network would fail with a dial error instead of the message expected here.
func (t *Tests) TagRefusesIncompleteArguments(ctx context.Context) error {
	client := dag.Oci().Registry("test-registry.invalid")
	digest := "sha256:" + strings.Repeat("a", 64)

	cases := []struct {
		repository, digest, tag string
		want, why               string
	}{
		{repository: "", digest: digest, tag: "v1", want: "repository is required", why: "no repository to tag in"},
		{repository: " ", digest: digest, tag: "v1", want: "repository is required", why: "a whitespace repository"},
		{repository: "app", digest: "", tag: "v1", want: "digest is required", why: "nothing to point the tag at"},
		{repository: "app", digest: digest, tag: "", want: "tag is required", why: "no name to give it"},
		{repository: "app", digest: digest, tag: " ", want: "tag is required", why: "a whitespace tag"},
	}
	for _, c := range cases {
		_, err := client.Tag(ctx, c.repository, c.digest, c.tag)
		if err == nil {
			return fmt.Errorf("Tag(%q, %q, %q) was accepted (%s)", c.repository, c.digest, c.tag, c.why)
		}
		if !strings.Contains(err.Error(), c.want) {
			return fmt.Errorf("Tag(%q, %q, %q) (%s): want a refusal carrying %q, got: %v",
				c.repository, c.digest, c.tag, c.why, c.want, err)
		}
	}
	return nil
}
