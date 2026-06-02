package main

import (
	"context"

	"dagger/java/internal/dagger"
)

// Gradle wraps the Gradle build lifecycle as Dagger functions. Construct via
// Java.Gradle(). The container is gradle:<ver>-jdk<jdk> with ~/.gradle/caches
// mounted as a shared cache volume; an in-repo `gradlew` is used unless
// DisableWrapper forces the image's system `gradle`. Every invocation passes
// --no-daemon for reproducibility.
type Gradle struct {
	// +private
	Source *dagger.Directory
	// +private
	JdkVersion string
	// +private
	DisableWrapper bool
}

// gradleUserHome is where the official gradle image's non-root `gradle` user
// keeps its caches.
const gradleUserHome = "/home/gradle/.gradle"

// Container returns the prepared Gradle container with source mounted at /work,
// the shared gradle-caches-cache mounted at ~/.gradle/caches (owned by the
// gradle user), GRADLE_USER_HOME set, and the working directory set to /work.
//
// +cache="session"
func (g *Gradle) Container(ctx context.Context) (*dagger.Container, error) {
	jdk := g.JdkVersion
	if jdk == "" {
		jdk = resolveJdkVersionFromSource(ctx, g.Source)
	}
	return dag.Container().
		From("gradle:"+defaultGradleVersion+"-jdk"+jdk).
		WithMountedCache(
			gradleUserHome+"/caches",
			dag.CacheVolume("gradle-caches-cache"),
			dagger.ContainerWithMountedCacheOpts{Owner: "gradle"},
		).
		WithEnvVariable("GRADLE_USER_HOME", gradleUserHome).
		WithMountedDirectory(workdir, g.Source).
		WithWorkdir(workdir), nil
}

// Tasks runs the given Gradle tasks (plus any extra args) against the source
// and returns stdout. It dispatches to the in-repo `gradlew` when present
// unless DisableWrapper is set, and always passes --no-daemon.
//
// +cache="session"
func (g *Gradle) Tasks(
	ctx context.Context,
	tasks []string,
	// +optional
	args []string,
) (string, error) {
	ctr, err := g.Container(ctx)
	if err != nil {
		return "", err
	}
	cmd, err := g.command(ctx, tasks, args)
	if err != nil {
		return "", err
	}
	return ctr.WithExec(cmd).Stdout(ctx)
}

// Build runs `gradle build` and returns the build directory.
//
// +cache="session"
func (g *Gradle) Build(ctx context.Context) (*dagger.Directory, error) {
	ctr, err := g.Container(ctx)
	if err != nil {
		return nil, err
	}
	cmd, err := g.command(ctx, []string{"build"}, nil)
	if err != nil {
		return nil, err
	}
	return ctr.WithExec(cmd).Directory(workdir + "/build"), nil
}

// Test runs `gradle test` and returns stdout.
//
// +cache="session"
func (g *Gradle) Test(ctx context.Context) (string, error) {
	ctr, err := g.Container(ctx)
	if err != nil {
		return "", err
	}
	cmd, err := g.command(ctx, []string{"test"}, nil)
	if err != nil {
		return "", err
	}
	return ctr.WithExec(cmd).Stdout(ctx)
}

// Assemble runs `gradle assemble` and returns the build/libs directory (the
// built jar lands there).
//
// +cache="session"
func (g *Gradle) Assemble(ctx context.Context) (*dagger.Directory, error) {
	ctr, err := g.Container(ctx)
	if err != nil {
		return nil, err
	}
	cmd, err := g.command(ctx, []string{"assemble"}, nil)
	if err != nil {
		return nil, err
	}
	return ctr.WithExec(cmd).Directory(workdir + "/build/libs"), nil
}

// command builds the argv for a Gradle invocation: `./gradlew` when an in-repo
// wrapper is present and not disabled, otherwise the system `gradle`.
// --no-daemon is always present.
func (g *Gradle) command(ctx context.Context, tasks, args []string) ([]string, error) {
	exe := "gradle"
	if !g.DisableWrapper {
		present, err := hasWrapper(ctx, g.Source, "gradlew")
		if err != nil {
			return nil, err
		}
		if present {
			exe = "./gradlew"
		}
	}
	cmd := []string{exe, "--no-daemon"}
	cmd = append(cmd, tasks...)
	cmd = append(cmd, args...)
	return cmd, nil
}
