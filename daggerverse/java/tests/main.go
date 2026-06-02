// Package main implements the test module for the java Dagger module. Each
// test is exposed as a standalone dagger function so it can be invoked
// individually during TDD; All wires them up for parallel execution under
// `dagger call all`.
package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/tests/internal/dagger"

	par "github.com/dagger/dagger/util/parallel"
)

type Tests struct{}

// All runs every java-module test in parallel.
//
// parallel caps how many tests run concurrently inside this suite. Defaults to
// 0 (unbounded fan-out) — each `dagger check` job runs on its own runner, so
// in-runner parallelism is bounded by the VM, not the scheduler.
//
// +check
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

	jobs = jobs.WithJob("ContainerHasJdk", t.ContainerHasJdk)
	jobs = jobs.WithJob("ToolVersionReportsJdk", t.ToolVersionReportsJdk)
	jobs = jobs.WithJob("RunJarPrintsOutput", t.RunJarPrintsOutput)
	jobs = jobs.WithJob("ContainerUsesPinnedJdkVersion", t.ContainerUsesPinnedJdkVersion)
	jobs = jobs.WithJob("ContainerInfersJdkFromJavaVersionFile", t.ContainerInfersJdkFromJavaVersionFile)
	jobs = jobs.WithJob("MavenInfersJdkFromPom", t.MavenInfersJdkFromPom)
	jobs = jobs.WithJob("GradleInfersJdkFromBuildGradle", t.GradleInfersJdkFromBuildGradle)
	jobs = jobs.WithJob("MavenContainerHasMaven", t.MavenContainerHasMaven)
	jobs = jobs.WithJob("MavenGoalsPassThrough", t.MavenGoalsPassThrough)
	jobs = jobs.WithJob("MavenCompileProducesClasses", t.MavenCompileProducesClasses)
	jobs = jobs.WithJob("MavenTestPasses", t.MavenTestPasses)
	jobs = jobs.WithJob("MavenPackageProducesJar", t.MavenPackageProducesJar)
	jobs = jobs.WithJob("GradleContainerHasGradle", t.GradleContainerHasGradle)
	jobs = jobs.WithJob("GradleTasksPassThrough", t.GradleTasksPassThrough)
	jobs = jobs.WithJob("GradleBuildProducesArtifacts", t.GradleBuildProducesArtifacts)
	jobs = jobs.WithJob("GradleTestPasses", t.GradleTestPasses)
	jobs = jobs.WithJob("GradleAssembleProducesJar", t.GradleAssembleProducesJar)
	jobs = jobs.WithJob("MavenUsesWrapperWhenPresent", t.MavenUsesWrapperWhenPresent)
	jobs = jobs.WithJob("MavenDisableWrapperUsesSystemMaven", t.MavenDisableWrapperUsesSystemMaven)
	jobs = jobs.WithJob("GradleUsesWrapperWhenPresent", t.GradleUsesWrapperWhenPresent)
	jobs = jobs.WithJob("GradleDisableWrapperUsesSystemGradle", t.GradleDisableWrapperUsesSystemGradle)
	jobs = jobs.WithJob("MavenPackageRunsTestsByDefault", t.MavenPackageRunsTestsByDefault)
	jobs = jobs.WithJob("MavenPackageSkipTestsBypassesFailingTest", t.MavenPackageSkipTestsBypassesFailingTest)

	return jobs.Run(ctx)
}

// ---- fixture loaders ----

func fixture(name string) *dagger.Directory {
	return dag.CurrentModule().Source().Directory("fixtures/" + name)
}

func mavenHelloDir() *dagger.Directory     { return fixture("maven-hello") }
func mavenJdk17Dir() *dagger.Directory     { return fixture("maven-jdk17") }
func mavenFailtestDir() *dagger.Directory  { return fixture("maven-failtest") }
func mavenWrapperDir() *dagger.Directory   { return fixture("maven-wrapper") }
func gradleHelloDir() *dagger.Directory    { return fixture("gradle-hello") }
func gradleJdk17Dir() *dagger.Directory    { return fixture("gradle-jdk17") }
func gradleWrapperDir() *dagger.Directory  { return fixture("gradle-wrapper") }
func dotJavaVersionDir() *dagger.Directory { return fixture("dotjava-version") }
func runJarDir() *dagger.Directory         { return fixture("run-jar") }

// javaVersionBanner runs `java -version` in ctr and returns its combined
// stdout+stderr (the JDK writes the banner to stderr).
func javaVersionBanner(ctx context.Context, ctr *dagger.Container) (string, error) {
	return ctr.WithExec([]string{"sh", "-c", "java -version 2>&1"}).Stdout(ctx)
}

// ---- JDK layer tests ----

// ContainerHasJdk proves the base container is reachable, source is mounted,
// and the JDK's `java` runs. Canary for every other test.
func (t *Tests) ContainerHasJdk(ctx context.Context) error {
	out, err := javaVersionBanner(ctx, dag.Java().Container(mavenHelloDir()))
	if err != nil {
		return fmt.Errorf("java -version exec: %w", err)
	}
	if !strings.Contains(out, "version \"") {
		return fmt.Errorf("expected a java version banner, got %q", out)
	}
	return nil
}

// ToolVersionReportsJdk asserts ToolVersion reports the pinned JDK major.
func (t *Tests) ToolVersionReportsJdk(ctx context.Context) error {
	out, err := dag.Java(dagger.JavaOpts{Version: "21"}).ToolVersion(ctx)
	if err != nil {
		return fmt.Errorf("tool version: %w", err)
	}
	if !strings.Contains(out, "\"21") {
		return fmt.Errorf("expected JDK 21 banner, got %q", out)
	}
	return nil
}

// RunJarPrintsOutput builds a runnable jar from the run-jar fixture inside the
// JDK container, then runs it via Java.Run and checks its stdout.
func (t *Tests) RunJarPrintsOutput(ctx context.Context) error {
	built := dag.Java().Container(runJarDir()).
		WithExec([]string{"javac", "Main.java"}).
		WithExec([]string{"jar", "cfe", "app.jar", "Main", "Main.class"}).
		Directory("/work")
	out, err := dag.Java().Run(ctx, built, "app.jar")
	if err != nil {
		return fmt.Errorf("run jar: %w", err)
	}
	if strings.TrimSpace(out) != "Hello from jar" {
		return fmt.Errorf("expected %q, got %q", "Hello from jar", out)
	}
	return nil
}

// ContainerUsesPinnedJdkVersion asserts New("17") overrides the pom's 21.
func (t *Tests) ContainerUsesPinnedJdkVersion(ctx context.Context) error {
	out, err := javaVersionBanner(ctx, dag.Java(dagger.JavaOpts{Version: "17"}).Container(mavenHelloDir()))
	if err != nil {
		return fmt.Errorf("java -version exec: %w", err)
	}
	if !strings.Contains(out, "\"17") {
		return fmt.Errorf("expected pinned JDK 17 banner, got %q", out)
	}
	return nil
}

// ContainerInfersJdkFromJavaVersionFile asserts .java-version (17) wins over
// the conflicting pom (21).
func (t *Tests) ContainerInfersJdkFromJavaVersionFile(ctx context.Context) error {
	out, err := javaVersionBanner(ctx, dag.Java().Container(dotJavaVersionDir()))
	if err != nil {
		return fmt.Errorf("java -version exec: %w", err)
	}
	if !strings.Contains(out, "\"17") {
		return fmt.Errorf("expected inferred JDK 17 banner, got %q", out)
	}
	return nil
}

// ---- inference via build tools ----

// MavenInfersJdkFromPom asserts the Maven container's JDK is inferred from pom.
func (t *Tests) MavenInfersJdkFromPom(ctx context.Context) error {
	out, err := javaVersionBanner(ctx, dag.Java().Maven(mavenJdk17Dir()).Container())
	if err != nil {
		return fmt.Errorf("java -version exec: %w", err)
	}
	if !strings.Contains(out, "\"17") {
		return fmt.Errorf("expected JDK 17 banner from pom inference, got %q", out)
	}
	return nil
}

// GradleInfersJdkFromBuildGradle asserts the Gradle container's JDK is inferred
// from build.gradle.
func (t *Tests) GradleInfersJdkFromBuildGradle(ctx context.Context) error {
	out, err := javaVersionBanner(ctx, dag.Java().Gradle(gradleJdk17Dir()).Container())
	if err != nil {
		return fmt.Errorf("java -version exec: %w", err)
	}
	if !strings.Contains(out, "\"17") {
		return fmt.Errorf("expected JDK 17 banner from build.gradle inference, got %q", out)
	}
	return nil
}

// ---- Maven tests ----

// MavenContainerHasMaven proves the Maven container has a working `mvn`.
func (t *Tests) MavenContainerHasMaven(ctx context.Context) error {
	out, err := dag.Java().Maven(mavenHelloDir()).Container().
		WithExec([]string{"mvn", "-version"}).Stdout(ctx)
	if err != nil {
		return fmt.Errorf("mvn -version: %w", err)
	}
	if !strings.Contains(out, "Apache Maven") {
		return fmt.Errorf("expected 'Apache Maven' in output, got %q", out)
	}
	return nil
}

// MavenGoalsPassThrough proves arbitrary goals/flags pass through to mvn.
func (t *Tests) MavenGoalsPassThrough(ctx context.Context) error {
	out, err := dag.Java().Maven(mavenHelloDir()).Goals(ctx, []string{"-version"})
	if err != nil {
		return fmt.Errorf("maven goals: %w", err)
	}
	if !strings.Contains(out, "Apache Maven") {
		return fmt.Errorf("expected 'Apache Maven' in output, got %q", out)
	}
	return nil
}

// MavenCompileProducesClasses asserts `mvn compile` produces .class files.
func (t *Tests) MavenCompileProducesClasses(ctx context.Context) error {
	target := dag.Java().Maven(mavenHelloDir()).Compile()
	classes, err := target.Glob(ctx, "**/*.class")
	if err != nil {
		return fmt.Errorf("glob classes: %w", err)
	}
	if len(classes) == 0 {
		return fmt.Errorf("expected compiled .class files under target, found none")
	}
	return nil
}

// MavenTestPasses asserts `mvn test` succeeds on the hello fixture.
func (t *Tests) MavenTestPasses(ctx context.Context) error {
	out, err := dag.Java().Maven(mavenHelloDir()).Test(ctx)
	if err != nil {
		return fmt.Errorf("maven test: %w", err)
	}
	if !strings.Contains(out, "BUILD SUCCESS") {
		return fmt.Errorf("expected BUILD SUCCESS, got %q", out)
	}
	return nil
}

// MavenPackageProducesJar asserts `mvn package` produces a jar.
func (t *Tests) MavenPackageProducesJar(ctx context.Context) error {
	target := dag.Java().Maven(mavenHelloDir()).Package()
	jars, err := target.Glob(ctx, "*.jar")
	if err != nil {
		return fmt.Errorf("glob jars: %w", err)
	}
	if len(jars) == 0 {
		return fmt.Errorf("expected a packaged jar under target, found none")
	}
	return nil
}

// ---- Gradle tests ----

// GradleContainerHasGradle proves the Gradle container has a working `gradle`.
func (t *Tests) GradleContainerHasGradle(ctx context.Context) error {
	out, err := dag.Java().Gradle(gradleHelloDir()).Container().
		WithExec([]string{"gradle", "--version"}).Stdout(ctx)
	if err != nil {
		return fmt.Errorf("gradle --version: %w", err)
	}
	if !strings.Contains(out, "Gradle") {
		return fmt.Errorf("expected 'Gradle' in output, got %q", out)
	}
	return nil
}

// GradleTasksPassThrough proves arbitrary tasks pass through to gradle.
func (t *Tests) GradleTasksPassThrough(ctx context.Context) error {
	out, err := dag.Java().Gradle(gradleHelloDir()).Tasks(ctx, []string{"tasks"})
	if err != nil {
		return fmt.Errorf("gradle tasks: %w", err)
	}
	if !strings.Contains(out, "BUILD SUCCESSFUL") {
		return fmt.Errorf("expected BUILD SUCCESSFUL, got %q", out)
	}
	return nil
}

// GradleBuildProducesArtifacts asserts `gradle build` produces artifacts.
func (t *Tests) GradleBuildProducesArtifacts(ctx context.Context) error {
	build := dag.Java().Gradle(gradleHelloDir()).Build()
	jars, err := build.Glob(ctx, "**/*.jar")
	if err != nil {
		return fmt.Errorf("glob build artifacts: %w", err)
	}
	if len(jars) == 0 {
		return fmt.Errorf("expected build artifacts (jar), found none")
	}
	return nil
}

// GradleTestPasses asserts `gradle test` succeeds on the hello fixture.
func (t *Tests) GradleTestPasses(ctx context.Context) error {
	out, err := dag.Java().Gradle(gradleHelloDir()).Test(ctx)
	if err != nil {
		return fmt.Errorf("gradle test: %w", err)
	}
	if !strings.Contains(out, "BUILD SUCCESSFUL") {
		return fmt.Errorf("expected BUILD SUCCESSFUL, got %q", out)
	}
	return nil
}

// GradleAssembleProducesJar asserts `gradle assemble` produces a jar.
func (t *Tests) GradleAssembleProducesJar(ctx context.Context) error {
	libs := dag.Java().Gradle(gradleHelloDir()).Assemble()
	jars, err := libs.Glob(ctx, "*.jar")
	if err != nil {
		return fmt.Errorf("glob libs: %w", err)
	}
	if len(jars) == 0 {
		return fmt.Errorf("expected an assembled jar under build/libs, found none")
	}
	return nil
}

// ---- wrapper tests ----

// MavenUsesWrapperWhenPresent asserts the in-repo mvnw is used by default.
func (t *Tests) MavenUsesWrapperWhenPresent(ctx context.Context) error {
	out, err := dag.Java().Maven(mavenWrapperDir()).Goals(ctx, []string{"validate"})
	if err != nil {
		return fmt.Errorf("maven validate via wrapper: %w", err)
	}
	if !strings.Contains(out, "MVNW-SHIM") {
		return fmt.Errorf("expected wrapper sentinel MVNW-SHIM, got %q", out)
	}
	return nil
}

// MavenDisableWrapperUsesSystemMaven asserts disableWrapper bypasses mvnw.
func (t *Tests) MavenDisableWrapperUsesSystemMaven(ctx context.Context) error {
	out, err := dag.Java().Maven(mavenWrapperDir(), dagger.JavaMavenOpts{DisableWrapper: true}).
		Goals(ctx, []string{"validate"})
	if err != nil {
		return fmt.Errorf("maven validate via system mvn: %w", err)
	}
	if strings.Contains(out, "MVNW-SHIM") {
		return fmt.Errorf("expected system mvn (no MVNW-SHIM), got %q", out)
	}
	if !strings.Contains(out, "BUILD SUCCESS") {
		return fmt.Errorf("expected BUILD SUCCESS from system mvn, got %q", out)
	}
	return nil
}

// GradleUsesWrapperWhenPresent asserts the in-repo gradlew is used by default.
func (t *Tests) GradleUsesWrapperWhenPresent(ctx context.Context) error {
	out, err := dag.Java().Gradle(gradleWrapperDir()).Tasks(ctx, []string{"help"})
	if err != nil {
		return fmt.Errorf("gradle help via wrapper: %w", err)
	}
	if !strings.Contains(out, "GRADLEW-SHIM") {
		return fmt.Errorf("expected wrapper sentinel GRADLEW-SHIM, got %q", out)
	}
	return nil
}

// GradleDisableWrapperUsesSystemGradle asserts disableWrapper bypasses gradlew.
func (t *Tests) GradleDisableWrapperUsesSystemGradle(ctx context.Context) error {
	out, err := dag.Java().Gradle(gradleWrapperDir(), dagger.JavaGradleOpts{DisableWrapper: true}).
		Tasks(ctx, []string{"help"})
	if err != nil {
		return fmt.Errorf("gradle help via system gradle: %w", err)
	}
	if strings.Contains(out, "GRADLEW-SHIM") {
		return fmt.Errorf("expected system gradle (no GRADLEW-SHIM), got %q", out)
	}
	if !strings.Contains(out, "BUILD SUCCESSFUL") {
		return fmt.Errorf("expected BUILD SUCCESSFUL from system gradle, got %q", out)
	}
	return nil
}

// ---- package/skipTests behavior ----

// MavenPackageRunsTestsByDefault asserts the failing test runs (package fails)
// when skipTests is not set.
func (t *Tests) MavenPackageRunsTestsByDefault(ctx context.Context) error {
	_, err := dag.Java().Maven(mavenFailtestDir()).Package().Sync(ctx)
	if err == nil {
		return fmt.Errorf("expected package to fail because the failing test ran, but it succeeded")
	}
	return nil
}

// MavenPackageSkipTestsBypassesFailingTest asserts skipTests produces a jar
// despite the failing test.
func (t *Tests) MavenPackageSkipTestsBypassesFailingTest(ctx context.Context) error {
	target := dag.Java().Maven(mavenFailtestDir()).Package(dagger.JavaMavenPackageOpts{SkipTests: true})
	jars, err := target.Glob(ctx, "*.jar")
	if err != nil {
		return fmt.Errorf("glob jars: %w", err)
	}
	if len(jars) == 0 {
		return fmt.Errorf("expected a packaged jar with skipTests, found none")
	}
	return nil
}
