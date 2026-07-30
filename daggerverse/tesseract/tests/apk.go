package main

// apk.go covers the air-gapped story: installing the module's packages from an
// Alpine repository the caller controls rather than from dl-cdn.alpinelinux.org.
//
// The repository the tests install from is a real one — a real package set, a
// real signed index, served over real HTTP by a Dagger service — because every
// part of what these tests are about lives in apk rather than in this module:
// whether a replaced /etc/apk/repositories is honoured, whether an index signed
// by an unknown key is refused, whether credentials reach the fetch. Asserting
// on the flags the module passes would test none of it.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"dagger/tests/internal/dagger"
)

const (
	// mirrorImage is what the mirror is built and served on: the same Alpine
	// release the module pins, so the packages the mirror carries are exactly
	// the packages the module would otherwise have fetched from the CDN.
	mirrorImage = "docker.io/library/alpine:3.24"

	// mirrorPackages is what the mirror has to carry to stand in for the
	// public repositories completely. That is the whole toolchain and nothing
	// else: this module performs exactly one `apk add`, rendering a document's
	// pages having moved to the pdf module along with the packages that did it.
	mirrorPackages = tesseractPkg + " " + tesseractDataPkg

	tesseractPkg     = "tesseract-ocr"
	tesseractDataPkg = "tesseract-ocr-data-eng"

	// mirrorBuildDir is the repository tree the build container assembles —
	// one architecture directory holding the packages and the signed index —
	// and mirrorKeyDir the keypair it signs with.
	mirrorBuildDir = "/mirror"
	mirrorKeyDir   = "/keys"

	// mirrorKeyName is the file name of the key the index is signed with, and
	// it is load-bearing at both ends: abuild-sign writes the name into the
	// index's signature and apk looks that exact file up in /etc/apk/keys. It
	// is what WithApkKey has to preserve, and the reason that option takes a
	// file rather than the key's bytes.
	mirrorKeyName = "tesseract-mirror.rsa.pub"
	mirrorKeyFile = mirrorKeyDir + "/tesseract-mirror.rsa"

	// mirrorRoot is the served document root and mirrorPath the path under it
	// the repository sits at. The repository is not at the root of the host on
	// purpose: a URL whose path is dropped would still resolve, and that is a
	// bug this suite should catch rather than tolerate.
	mirrorRoot = "/srv"
	mirrorPath = "alpine"

	// mirrorPort is where the mirror listens. It is above 1024 so the server
	// needs no privileges it would not have anyway.
	mirrorPort = 8080

	// mirrorUser is the account the authenticated mirror requires. Its
	// password is generated per run rather than written down here.
	mirrorUser = "tesseract"

	// httpdPkg is the server the mirror is published with. Alpine's busybox
	// carries no httpd applet, and what this needs beyond serving a directory
	// is one flag's worth of basic authentication — which darkhttpd has and
	// nginx would want a config file and a password-hashing tool for.
	httpdPkg = "darkhttpd"

	// apkRepositoriesFile is the list `apk add` resolves packages from, and
	// therefore the file that says whether the image's defaults survived.
	apkRepositoriesFile = "/etc/apk/repositories"

	// alpineCdnHost appears in every repository line a stock Alpine image
	// ships, and in none that WithApkRepository writes.
	alpineCdnHost = "dl-cdn.alpinelinux.org"

	// apkNetrcEnv is the variable WithApkAuth points at its mounted
	// credentials, and apkNetrcPath the mount it names. An image built without
	// that option must carry neither.
	apkNetrcEnv  = "NETRC"
	apkNetrcPath = "/run/apk/netrc"
)

// mirrorBuildScript assembles the private repository.
//
// The packages are fetched with their whole dependency closure rather than
// listed by hand: a mirror missing one transitive library fails the same way a
// missing repository does, and would make these tests assert on the wrong
// thing.
//
// `--rewrite-arch` is not decoration. apk resolves a package's path as
// `$base/$arch/$name-$version.apk` using the *package's* architecture, so the
// `noarch` packages in the closure — the tesseract language data among them —
// would be looked for under a `noarch/` directory that no mirror has. Alpine's
// own index generation rewrites them for the same reason; without it, the two
// largest packages in the set fail to install and everything else succeeds,
// which is a failure shaped exactly like a flake.
//
// The index is signed because an unsigned one is not a lesser mirror, it is an
// unusable one: apk refuses an index it cannot verify rather than installing
// from it, and `--allow-untrusted` is deliberately not something this module
// offers.
const mirrorBuildScript = `set -e
arch=$(apk --print-arch)
apk update
apk add abuild openssl
mkdir -p ` + mirrorBuildDir + `/$arch ` + mirrorKeyDir + `
openssl genrsa -out ` + mirrorKeyFile + ` 2048
openssl rsa -in ` + mirrorKeyFile + ` -pubout -out ` + mirrorKeyDir + `/` + mirrorKeyName + `
apk fetch --recursive --output ` + mirrorBuildDir + `/$arch ` + mirrorPackages + `
cd ` + mirrorBuildDir + `/$arch
apk index --rewrite-arch $arch --output APKINDEX.tar.gz *.apk
abuild-sign -k ` + mirrorKeyFile + ` -p ` + mirrorKeyName + ` APKINDEX.tar.gz`

// ------------------------------------------------------------------ the mirror

// apkMirror is a private Alpine repository standing in for the one an
// air-gapped network would run: a signed package set on a host that is not the
// CDN, optionally behind credentials.
type apkMirror struct {
	// URL is the repository base, spelled the way it would be spelled in
	// /etc/apk/repositories.
	URL string
	// Key is the public half of what the index was signed with, named the way
	// the signature names it.
	Key *dagger.File
	// Auth carries the mirror's credentials in netrc form, and is nil for a
	// mirror that asks for none.
	Auth *dagger.Secret
	// Password is the plaintext inside Auth, kept so a test can go looking for
	// it in places it must never turn up.
	Password string
}

// startApkMirror builds the repository, serves it, and returns what a caller
// needs to install from it.
//
// The service is started here rather than left to the first use: nothing binds
// it into the module's containers, so it is reached by the hostname the engine
// gives a running service, and that hostname does not exist until it runs.
func startApkMirror(ctx context.Context, authenticated bool) (*apkMirror, error) {
	build, err := mirrorBuild(ctx)
	if err != nil {
		return nil, err
	}

	srv := dag.Container().
		From(mirrorImage).
		WithExec([]string{"apk", "add", "--no-cache", httpdPkg}).
		WithDirectory(mirrorRoot+"/"+mirrorPath, build.Directory(mirrorBuildDir)).
		WithExposedPort(mirrorPort)
	args := []string{httpdPkg, mirrorRoot, "--port", strconv.Itoa(mirrorPort)}

	var password string
	if authenticated {
		if password, err = randomPassword(); err != nil {
			return nil, err
		}
		args = append(args, "--auth", mirrorUser+":"+password)
	}

	// The server goes in Args rather than in a WithExec: a server does not
	// exit, so building it as a step would hang before ever becoming a
	// service.
	svc := srv.AsService(dagger.ContainerAsServiceOpts{Args: args})
	if _, err := svc.Start(ctx); err != nil {
		return nil, fmt.Errorf("start the apk mirror: %w", err)
	}
	host, err := svc.Hostname(ctx)
	if err != nil {
		return nil, fmt.Errorf("apk mirror hostname: %w", err)
	}

	mirror := &apkMirror{
		URL: fmt.Sprintf("http://%s:%d/%s", host, mirrorPort, mirrorPath),
		Key: build.File(mirrorKeyDir + "/" + mirrorKeyName),
	}
	if authenticated {
		// The stanza names the host and not the port, which is all libfetch
		// matches on. The secret's name is unique per run because two secrets
		// sharing a name in one session are one secret, and every
		// authenticated mirror here has its own password.
		nonce, err := randomPassword()
		if err != nil {
			return nil, err
		}
		mirror.Auth = dag.SetSecret(
			"apk-mirror-netrc-"+nonce,
			fmt.Sprintf("machine %s login %s password %s\n", host, mirrorUser, password))
		mirror.Password = password
	}
	return mirror, nil
}

// mirrorBuild returns the container the repository and its keypair are lifted
// out of.
//
// It is resolved to an ID and loaded back before anything is selected off it,
// because the build generates a keypair: two selections off an unpinned
// generator are two evaluations, and a repository signed by one key with the
// other key's public half published beside it is a mirror nothing can install
// from.
func mirrorBuild(ctx context.Context) (*dagger.Container, error) {
	id, err := dag.Container().
		From(mirrorImage).
		WithExec([]string{"sh", "-c", mirrorBuildScript}).
		ID(ctx)
	if err != nil {
		return nil, fmt.Errorf("build the apk mirror: %w", err)
	}
	return dag.LoadContainerFromID(dagger.ContainerID(id)), nil
}

// apply points the module at this mirror and at nothing else.
func (m *apkMirror) apply(t *dagger.Tesseract) *dagger.Tesseract {
	t = t.WithApkRepository(m.URL).WithApkKey(m.Key)
	if m.Auth != nil {
		t = t.WithApkAuth(m.Auth)
	}
	return t
}

// randomPassword returns a credential that exists only for the run that
// generated it, so no test password is ever committed.
func randomPassword() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate a mirror password: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// ------------------------------------------------------------------- the tests

// ApkRepositoryInstallsFromPrivateMirror asserts the toolchain image can be
// assembled entirely out of a caller-supplied repository, which is the whole
// point of the option: with the mirror configured, the packages come from it.
func (t *Tests) ApkRepositoryInstallsFromPrivateMirror(ctx context.Context) error {
	mirror, err := startApkMirror(ctx, false)
	if err != nil {
		return err
	}
	got, err := mirror.apply(ocr()).Version(ctx)
	if err != nil {
		return fmt.Errorf("Version off a private mirror: %w", err)
	}
	if !strings.HasPrefix(got, "5.") {
		return fmt.Errorf("expected a 5.x tesseract version off the mirror, got %q", got)
	}
	return nil
}

// ApkRepositoryReplacesImageDefaults asserts the first WithApkRepository
// replaces the image's repository list rather than appending to it.
//
// Appending would look identical in every test above — the packages would
// still install, from the mirror or from the CDN, and nothing would say which.
// The difference only shows on the network this option exists for, where a
// surviving default is a repository apk waits on until it times out. So the
// assertion is on the file, and it is that the CDN is *gone*.
func (t *Tests) ApkRepositoryReplacesImageDefaults(ctx context.Context) error {
	mirror, err := startApkMirror(ctx, false)
	if err != nil {
		return err
	}
	got, err := readRepositories(ctx, mirror.apply(ocr()))
	if err != nil {
		return err
	}
	if strings.Contains(got, alpineCdnHost) {
		return fmt.Errorf("expected %s gone from %s, got:\n%s", alpineCdnHost, apkRepositoriesFile, got)
	}
	if strings.TrimSpace(got) != mirror.URL {
		return fmt.Errorf("expected %s to hold only %q, got:\n%s", apkRepositoriesFile, mirror.URL, got)
	}
	return nil
}

// UntrustedIndexIsRejectedWithoutApkKey asserts a repository whose index is
// signed by a key the image does not trust is refused rather than installed
// from, so WithApkKey is doing verification and not decoration. It is also
// what says `--allow-untrusted` is genuinely not on offer: a module that
// quietly installed from an unverifiable index would make the air-gapped path
// the least trustworthy one.
//
// The same mirror is installed from twice, with the key and without it, so the
// key is the only difference between the two outcomes. Asserting only that the
// second fails would be satisfied by a mirror that was broken outright — and
// asserting on apk's own wording (it says `UNTRUSTED signature`) is not
// available: an exec failure crosses the module boundary as its exit status,
// with the output left in the logs.
func (t *Tests) UntrustedIndexIsRejectedWithoutApkKey(ctx context.Context) error {
	mirror, err := startApkMirror(ctx, false)
	if err != nil {
		return err
	}
	if _, err := mirror.apply(ocr()).Version(ctx); err != nil {
		return fmt.Errorf("Version off a mirror whose key was supplied: %w", err)
	}
	if _, err := ocr().WithApkRepository(mirror.URL).Version(ctx); err == nil {
		return fmt.Errorf("expected an index signed by an untrusted key to be refused, got a working image")
	}
	return nil
}

// ApkAuthInstallsFromAuthenticatedRepository asserts credentials reach the
// fetch, and that they reach it without becoming part of the image.
//
// The second half is the reason the option takes a Secret: a mirror password
// spelled into a repository URL would land in /etc/apk/repositories, in every
// apk error quoting it, and in the layer a caller exports.
func (t *Tests) ApkAuthInstallsFromAuthenticatedRepository(ctx context.Context) error {
	mirror, err := startApkMirror(ctx, true)
	if err != nil {
		return err
	}
	ctr := mirror.apply(ocr()).Container()
	if _, err := ctr.WithExec([]string{"tesseract", "--version"}).Stdout(ctx); err != nil {
		return fmt.Errorf("install from an authenticated mirror: %w", err)
	}

	// The credentials are mounted rather than written, so the assembled
	// image's own filesystem carries nothing under the mount point.
	leaked, err := ctr.Directory("/").Glob(ctx, strings.TrimPrefix(apkNetrcPath, "/"))
	if err != nil {
		return fmt.Errorf("look for %s in the assembled image: %w", apkNetrcPath, err)
	}
	if len(leaked) > 0 {
		return fmt.Errorf("expected %s to be a mount and not a layer, found %v", apkNetrcPath, leaked)
	}

	// What the environment carries is the path, never the credential.
	env, err := ctr.EnvVariables(ctx)
	if err != nil {
		return fmt.Errorf("read the assembled image's environment: %w", err)
	}
	for _, v := range env {
		name, err := v.Name(ctx)
		if err != nil {
			return fmt.Errorf("read an environment variable's name: %w", err)
		}
		value, err := v.Value(ctx)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if name == apkNetrcEnv && value != apkNetrcPath {
			return fmt.Errorf("expected %s to name the mount %q, got %q", apkNetrcEnv, apkNetrcPath, value)
		}
		if strings.Contains(value, mirror.Password) {
			return fmt.Errorf("expected no credentials in the image's environment, %s carries them", name)
		}
	}
	return nil
}

// AuthenticatedRepositoryIsRejectedWithoutApkAuth asserts the authenticated
// mirror really is authenticated — that the test above passes because the
// credentials were supplied and used, and not because the server never asked
// for any. Both installs run against the one mirror, so WithApkAuth is the
// only difference between them.
func (t *Tests) AuthenticatedRepositoryIsRejectedWithoutApkAuth(ctx context.Context) error {
	mirror, err := startApkMirror(ctx, true)
	if err != nil {
		return err
	}
	if _, err := mirror.apply(ocr()).Version(ctx); err != nil {
		return fmt.Errorf("Version off an authenticated mirror with credentials: %w", err)
	}
	if _, err := ocr().WithApkRepository(mirror.URL).WithApkKey(mirror.Key).Version(ctx); err == nil {
		return fmt.Errorf("expected a fetch with no credentials to be refused, got a working image")
	}
	return nil
}

// DefaultApkConfigurationIsUntouched asserts an image built without any of
// these options is the image this module built before they existed: the
// stock repository list, and no credential plumbing at all.
//
// It is the guard against the cheap implementation of all of the above —
// writing a repositories file, or setting the credential variable,
// unconditionally — which would work for the mirror and rebuild the world for
// every existing caller.
func (t *Tests) DefaultApkConfigurationIsUntouched(ctx context.Context) error {
	got, err := readRepositories(ctx, ocr())
	if err != nil {
		return err
	}
	if !strings.Contains(got, alpineCdnHost) {
		return fmt.Errorf("expected the image's own repositories to survive, got:\n%s", got)
	}
	env, err := ocr().Container().WithExec([]string{"sh", "-c", `printf %s "$` + apkNetrcEnv + `"`}).Stdout(ctx)
	if err != nil {
		return fmt.Errorf("read %s from an unconfigured image: %w", apkNetrcEnv, err)
	}
	if env != "" {
		return fmt.Errorf("expected no %s on an unconfigured image, got %q", apkNetrcEnv, env)
	}
	return nil
}

// readRepositories reports the repository list of an assembled image, which is
// what says where its packages came from.
func readRepositories(ctx context.Context, t *dagger.Tesseract) (string, error) {
	out, err := t.Container().WithExec([]string{"cat", apkRepositoriesFile}).Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", apkRepositoriesFile, err)
	}
	return out, nil
}
