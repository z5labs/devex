package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"dagger/tests/internal/dagger"
)

// proxyImage is the reverse proxy the broken-referrers registry is built out
// of. Pinned for the same reason every other image here is: ":latest" would
// make a red suite ambiguous between "this module broke" and "nginx changed".
const proxyImage = "nginx:1.29-alpine"

// brokenReferrersProxy is a registry that accepts images and refuses
// referrers.
//
// The refusal is aimed at the referrers *tag* schema — the index stored under
// "sha256-<hex>" of the subject's digest — because that is the one request in
// the whole publish whose path can be told apart from every other by its URL
// alone. An image manifest, a referrer manifest and a tag move are all
// `PUT /v2/<repo>/manifests/<something>`; only the referrers index carries a
// digest spelled with a hyphen instead of a colon. A registry with no
// referrers API sends oras down that schema for every attach, which is
// precisely how devex#360 happened: GHCR answered 405 to the housekeeping and
// failed an attach whose documents had already landed.
//
// 405 rather than 500 for the same reason: it is the status GHCR really
// returned. `location ~` beats the prefix `location /` in nginx, so the
// refusal takes precedence over the proxy without either being conditional.
//
// The config is written to conf.d rather than to templates/ so the image's
// entrypoint runs no envsubst over it; $http_host has to survive to reach
// nginx, and a substitution pass would blank it and leave the upstream
// generating blob-upload Locations pointing at the wrong host.
const brokenReferrersProxy = `server {
    listen 5000;
    server_name _;
    client_max_body_size 0;

    location ~ "/manifests/sha256-" {
        return 405;
    }

    location / {
        proxy_pass http://registry:5000;
        proxy_set_header Host $http_host;
        proxy_request_buffering off;
        proxy_read_timeout 300s;
    }
}
`

// brokenReferrersRegistry is localRegistry with that proxy in front of it: a
// registry a publish can push an image to and cannot attach anything to.
//
// It returns the same four values localRegistry does, so every helper in this
// suite — curlProbeManifest, listTags, testRegistry, publishable — works
// against it unchanged. The proxy listens on 5000 for that reason.
//
// NONCE is why each caller gets its own instance: Dagger content-addresses
// services, so two identical container definitions are one running service
// shared across tests and across sessions, and one test's pushes would land in
// another's registry.
func brokenReferrersRegistry(ctx context.Context) (*dagger.Service, string, *dagger.Secret, error) {
	upstream, pwdHex, secret, err := localRegistry(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	nonce, err := dag.Random().Sha256(ctx)
	if err != nil {
		return nil, "", nil, fmt.Errorf("random sha256 (proxy nonce): %v", err)
	}
	svc := dag.Container().From(proxyImage).
		WithEnvVariable("NONCE", nonce).
		WithServiceBinding("registry", upstream).
		WithNewFile("/etc/nginx/conf.d/default.conf", brokenReferrersProxy).
		WithExposedPort(5000).
		AsService(dagger.ContainerAsServiceOpts{
			UseEntrypoint: true,
			Args:          []string{"nginx", "-g", "daemon off;"},
		})
	return svc, pwdHex, secret, nil
}

// untaggedDigest picks the untagged manifest's digest out of a failure
// message, which is where the only handle on it is: nothing names it, so a
// test that wants to look at what a failed publish left behind has to read the
// digest out of what the failure said.
//
// It is anchored to the clause Publish writes rather than matching any digest
// in the message, and that is load bearing. The failure reads
// "<attach error>; <digest> was left untagged in <repo>...", and the attach
// error names digests of its own — the subject it was attaching to, and on a
// later document the referrers that already landed. A bare sha256 match would
// take the first of those, and the probe below would then confirm that some
// manifest is present while proving nothing about the image. Requiring the
// clause also makes this fail loudly if the message ever stops carrying the
// digest, instead of quietly latching onto a different one.
var untaggedDigest = regexp.MustCompile(`(sha256:[0-9a-f]{64}) was left untagged`)

// tagListing returns the tags a repository holds, treating a repository the
// registry will not list at all as holding none.
//
// listTags is the suite's usual reader and cannot be used here: it runs
// `curl -fs`, and registry:2 answers /v2/<name>/tags/list with 404
// NAME_UNKNOWN for a repository whose only manifests are untagged — measured
// against registry:2 here, on exactly the repository a failed publish leaves
// behind. That 404 is the strongest possible form of the answer this test
// wants, so it is a result rather than an error; a hard failure to reach the
// registry still is one, which is why the status is read rather than the exit
// code.
func tagListing(ctx context.Context, svc *dagger.Service, host, user, pwd, image string) ([]string, error) {
	out, err := dag.Container().From(curlImage).
		WithServiceBinding(host, svc).
		WithExec([]string{"sh", "-c", fmt.Sprintf(
			`curl -s -w '\n%%{http_code}' -u %s:%s http://%s:5000/v2/%s/tags/list`,
			user, pwd, host, image,
		)}).
		Stdout(ctx)
	if err != nil {
		return nil, fmt.Errorf("list the tags of %s: %v", image, err)
	}
	body, code, ok := strings.Cut(strings.TrimRight(out, "\n"), "\n")
	if !ok {
		// A body-less response leaves the status alone on the only line.
		body, code = "", strings.TrimSpace(out)
	}
	switch strings.TrimSpace(code) {
	case "404":
		return nil, nil
	case "200":
		tags, err := parseTagsList(body)
		if err != nil {
			return nil, err
		}
		// A referrers index is not a tag this pipeline published; see
		// withoutReferrerTags.
		return withoutReferrerTags(tags), nil
	default:
		return nil, fmt.Errorf("listing the tags of %s returned %s (body %s)", image, code, body)
	}
}

// AppPublishLeavesNoTagWhenAttachFails asserts that a publish whose attach
// fails leaves nothing a consumer can pull.
//
// This is devex#361's acceptance criterion and it is deliberately not
// "the run goes red". The run went red in devex#360 too; what made that
// failure worth an issue is that the image was tagged and pullable and
// carried no attestations, so nobody resolving the tag could tell a failed
// attach from a registry where nothing is ever attested. Asserting on the exit
// status would have passed against exactly that behaviour, which is why the
// assertions below are all about what the registry holds afterwards.
//
// Two registry states are covered, because a failed publish means different
// things in each:
//
//   - A version nobody has published: nothing may name the digest, so no tag
//     resolves at all and the tag listing is empty.
//   - A version already published: the old tag has to be exactly where it was,
//     still pointing at the release that did attest. A publish that moved the
//     tag first and then failed would have replaced a good release with an
//     unattested one, which is worse than the fresh case rather than milder.
func (t *Tests) AppPublishLeavesNoTagWhenAttachFails(ctx context.Context) error {
	const (
		version   = "v4.2.0"
		fresh     = "hello"
		published = "z5labs/hello-published"
	)
	src, err := gitFixture(ctx, helloDir(), "main", nil)
	if err != nil {
		return fmt.Errorf("gitFixture: %v", err)
	}
	svc, pwdHex, secret, err := brokenReferrersRegistry(ctx)
	if err != nil {
		return err
	}
	prov, err := newProvenanceHarness(ctx, "")
	if err != nil {
		return err
	}

	// Which failure path this test ran over is a checked fact, not a claim in
	// the comment above. A stand-in that quietly started serving referrers
	// would make every assertion below pass over a publish that never failed.
	hex, err := dag.Random().Sha256(ctx)
	if err != nil {
		return fmt.Errorf("random sha256 (referrers tag): %v", err)
	}
	code, err := curlProbeManifest(ctx, svc, registryAlias, "ci", pwdHex, fresh, "sha256-"+hex)
	if err != nil {
		return fmt.Errorf("probe the referrers tag: %v", err)
	}
	if code != 405 {
		return fmt.Errorf("the stand-in answered the referrers tag with %d, want 405: "+
			"it is not refusing referrers, so this test would pass over a publish that succeeded", code)
	}

	app := dag.Z5Labs().Go(src).App(version, dagger.Z5LabsGoChainAppOpts{Platforms: []dagger.Platform{hostPlatform()}})
	configured := publishable(app, svc, secret, prov)

	// A repository nobody has published to.
	_, pubErr := configured.Publish(ctx, []string{fresh})
	if pubErr == nil {
		return fmt.Errorf("expected Publish to fail against a registry that refuses referrers, got nil")
	}
	// The failure has to be the attach. A publish that fell over on the push
	// would leave no tag either, and would prove nothing about the ordering.
	if !strings.Contains(pubErr.Error(), "attach") {
		return fmt.Errorf("expected the failure to name the attach, got: %s", pubErr.Error())
	}
	// And it has to say what it left behind, because "publish failed" beside a
	// manifest that is genuinely in the registry is what a caller has to be
	// able to reason about.
	if !strings.Contains(pubErr.Error(), "untagged") {
		return fmt.Errorf("expected the failure to say the digest was left untagged, got: %s", pubErr.Error())
	}

	code, err = curlProbeManifest(ctx, svc, registryAlias, "ci", pwdHex, fresh, version)
	if err != nil {
		return fmt.Errorf("curl probe %s:%s: %v", fresh, version, err)
	}
	if code == 200 {
		return fmt.Errorf("the attach failed but %s:%s is pullable: a consumer cannot tell it carries no attestations", fresh, version)
	}
	// The tag listing rather than that one probe is what rules out a tag under
	// some other name — a publish deriving a tag from the branch or the commit
	// would leave the version 404 and still be reachable.
	tags, err := tagListing(ctx, svc, registryAlias, "ci", pwdHex, fresh)
	if err != nil {
		return err
	}
	if len(tags) != 0 {
		return fmt.Errorf("the attach failed but %s carries the tags %v, want none", fresh, tags)
	}

	// The other half of "an untagged manifest nothing resolves to": the
	// manifest really is there. Without this, every assertion above would hold
	// just as well for a publish that never pushed anything at all, and the
	// ordering under test — push first, name it last — would be indistinguishable
	// from a push that had simply been skipped. The digest comes out of the
	// failure message, which is the only place a caller can get it either.
	match := untaggedDigest.FindStringSubmatch(pubErr.Error())
	if match == nil {
		return fmt.Errorf("the failure names no untagged digest, so nothing can check what was left behind: %s", pubErr.Error())
	}
	digest := match[1]
	code, err = curlProbeManifest(ctx, svc, registryAlias, "ci", pwdHex, fresh, digest)
	if err != nil {
		return fmt.Errorf("curl probe %s@%s: %v", fresh, digest, err)
	}
	if code != 200 {
		return fmt.Errorf("the failure said %s was left untagged in %s, but the registry answers %d for it",
			digest, fresh, code)
	}

	// A repository where this version was already published. The stand-in
	// refuses referrers, so the incumbent is pushed straight through the oci
	// module rather than by a publish — what it stands for is the last release
	// that did attest, and all this test needs of it is that it is different
	// bytes under the same name.
	incumbent, err := testRegistry(svc, secret).PushImage(ctx, published, version,
		[]*dagger.Container{dag.Container().From(alpineImage)})
	if err != nil {
		return fmt.Errorf("seed %s:%s: %v", published, version, err)
	}
	_, pubErr = configured.Publish(ctx, []string{published})
	if pubErr == nil {
		return fmt.Errorf("expected Publish over an existing version to fail against a registry that refuses referrers, got nil")
	}
	// The same two checks as the fresh case, and for a stronger reason. An
	// incumbent tag is unmoved by any failure that happens before the tag is
	// written — a refused target, a credential, a build error — so without
	// pinning the failure to the attach, this half would stay green over a
	// publish that never reached the registry at all and would assert nothing
	// about the ordering.
	if !strings.Contains(pubErr.Error(), "attach") {
		return fmt.Errorf("expected the failure over an existing version to name the attach, got: %s", pubErr.Error())
	}
	if !strings.Contains(pubErr.Error(), "untagged") {
		return fmt.Errorf("expected the failure over an existing version to say the digest was left untagged, got: %s", pubErr.Error())
	}
	resolved, err := testRegistry(svc, secret).Resolve(ctx, published, version)
	if err != nil {
		return fmt.Errorf("resolve %s:%s after the failed publish: %v", published, version, err)
	}
	if resolved != incumbent {
		return fmt.Errorf("the failed publish moved %s:%s from %s to %s: a release that attested was replaced by one that did not",
			published, version, incumbent, resolved)
	}
	return nil
}
