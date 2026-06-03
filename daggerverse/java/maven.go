package main

import (
	"context"
	"slices"

	"dagger/java/internal/dagger"
)

// Maven wraps the Maven build lifecycle as Dagger functions. Construct via
// Java.Maven(). The container is maven:<ver>-eclipse-temurin-<jdk> with
// ~/.m2/repository mounted as a shared cache volume; an in-repo `mvnw` is used
// unless DisableWrapper forces the image's system `mvn`.
type Maven struct {
	// +private
	Source *dagger.Directory
	// +private
	JdkVersion string
	// +private
	DisableWrapper bool
}

// Container returns the prepared Maven container with source mounted at /work,
// the shared maven-repository-cache mounted at /root/.m2/repository, and the
// working directory set to /work.
//
// +cache="session"
func (m *Maven) Container(ctx context.Context) (*dagger.Container, error) {
	jdk := m.JdkVersion
	if jdk == "" {
		jdk = resolveJdkVersionFromSource(ctx, m.Source)
	}
	return dag.Container().
		From("maven:"+defaultMavenVersion+"-eclipse-temurin-"+jdk).
		WithMountedCache("/root/.m2/repository", dag.CacheVolume("maven-repository-cache")).
		WithMountedDirectory(workdir, m.Source).
		WithWorkdir(workdir), nil
}

// Goals runs the given Maven goals (plus any extra args) against the source
// and returns stdout. It dispatches to the in-repo `mvnw` when present unless
// DisableWrapper is set.
//
// +cache="session"
func (m *Maven) Goals(
	ctx context.Context,
	goals []string,
	// +optional
	args []string,
) (string, error) {
	ctr, err := m.Container(ctx)
	if err != nil {
		return "", err
	}
	cmd, err := m.command(ctx, goals, args)
	if err != nil {
		return "", err
	}
	return ctr.WithExec(cmd).Stdout(ctx)
}

// Compile runs `mvn compile` and returns the target directory (compiled
// classes land under target/classes).
//
// +cache="session"
func (m *Maven) Compile(ctx context.Context) (*dagger.Directory, error) {
	ctr, err := m.Container(ctx)
	if err != nil {
		return nil, err
	}
	cmd, err := m.command(ctx, []string{"compile"}, nil)
	if err != nil {
		return nil, err
	}
	return ctr.WithExec(cmd).Directory(workdir + "/target"), nil
}

// Test runs `mvn test` and returns stdout.
//
// +cache="session"
func (m *Maven) Test(ctx context.Context) (string, error) {
	ctr, err := m.Container(ctx)
	if err != nil {
		return "", err
	}
	cmd, err := m.command(ctx, []string{"test"}, nil)
	if err != nil {
		return "", err
	}
	return ctr.WithExec(cmd).Stdout(ctx)
}

// Package runs `mvn package` and returns the target directory (the built jar
// lands under target/). Tests run by default; skipTests passes -DskipTests so
// test sources still compile but are not executed.
//
// +cache="session"
func (m *Maven) Package(
	ctx context.Context,
	// +default=false
	skipTests bool,
) (*dagger.Directory, error) {
	ctr, err := m.Container(ctx)
	if err != nil {
		return nil, err
	}
	args := []string(nil)
	if skipTests {
		args = []string{"-DskipTests"}
	}
	cmd, err := m.command(ctx, []string{"package"}, args)
	if err != nil {
		return nil, err
	}
	return ctr.WithExec(cmd).Directory(workdir + "/target"), nil
}

// Verify runs `mvn verify` and returns stdout.
//
// +cache="session"
func (m *Maven) Verify(ctx context.Context) (string, error) {
	ctr, err := m.Container(ctx)
	if err != nil {
		return "", err
	}
	cmd, err := m.command(ctx, []string{"verify"}, nil)
	if err != nil {
		return "", err
	}
	return ctr.WithExec(cmd).Stdout(ctx)
}

// command builds the argv for a Maven invocation: `./mvnw` when an in-repo
// wrapper is present and not disabled, otherwise the system `mvn`.
func (m *Maven) command(ctx context.Context, goals, args []string) ([]string, error) {
	exe := "mvn"
	if !m.DisableWrapper {
		present, err := hasWrapper(ctx, m.Source, "mvnw")
		if err != nil {
			return nil, err
		}
		if present {
			exe = "./mvnw"
		}
	}
	cmd := []string{exe}
	cmd = append(cmd, goals...)
	cmd = append(cmd, args...)
	return cmd, nil
}

// hasWrapper reports whether name (e.g. "mvnw"/"gradlew") is a top-level entry
// in source.
func hasWrapper(ctx context.Context, source *dagger.Directory, name string) (bool, error) {
	entries, err := source.Entries(ctx)
	if err != nil {
		return false, err
	}
	return slices.Contains(entries, name), nil
}
