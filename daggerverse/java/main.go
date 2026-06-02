// Package main implements the java Dagger module: a thin wrapper around the
// JVM toolchain (the JDK plus the Maven and Gradle build tools) so downstream
// pipelines can compile, test, and package Java projects without re-inventing
// JDK pinning, build-tool selection, and cache plumbing.
//
// The JDK version is pinned via New(version) or inferred from the source's
// .java-version, then pom.xml, then build.gradle(.kts); it falls back to the
// module-pinned LTS default. Maven and Gradle are surfaced as cooperating
// objects via Java.Maven / Java.Gradle. The build-tool version is not taken
// from New(): an in-repo wrapper (mvnw/gradlew) is used when present, pinning
// the tool per-repo; disableWrapper forces the image's system tool.
package main

import (
	"context"
	"regexp"
	"strings"

	"dagger/java/internal/dagger"
)

const (
	// defaultJdkVersion is the module-pinned LTS used when New("") is called
	// against a source that declares no JDK version anywhere.
	defaultJdkVersion = "21"
	// defaultMavenVersion is the Maven release whose official image tag backs
	// Maven.Container. The matching tag is maven:<ver>-eclipse-temurin-<jdk>.
	defaultMavenVersion = "3.9.9"
	// defaultGradleVersion is the Gradle release whose official image tag backs
	// Gradle.Container. The matching tag is gradle:<ver>-jdk<jdk>.
	defaultGradleVersion = "8.14.5"

	// workdir is where source is mounted in every container.
	workdir = "/work"
)

// Java wraps the JVM toolchain as Dagger functions. Construct via New(); call
// Container() for the prepared JDK container, ToolVersion() for the pinned
// JDK banner, Run() to run a jar, or Maven()/Gradle() for the build-tool
// objects.
type Java struct {
	// Version is the pinned JDK major version (e.g. "21"). Empty means infer
	// from source (.java-version, then pom.xml / build.gradle); falls back to
	// the module-pinned LTS default.
	Version string
}

// New returns a Java module configured for the given JDK version.
// version is optional: empty means the version is inferred from the source for
// source-bearing funcs, and the module-pinned LTS default is used otherwise.
func New(
	// +optional
	version string,
) *Java {
	return &Java{Version: version}
}

// Container returns the prepared JDK container with source mounted at /work
// and the working directory set to /work. Use this as an escape hatch when a
// JVM command isn't covered by the typed helpers.
//
// The base image is eclipse-temurin:<jdk>-jdk where jdk comes from New() or,
// when New("") was used, from source (.java-version, then pom.xml, then
// build.gradle(.kts)), falling back to the module-pinned LTS default. The
// signature takes ctx + returns error because source inspection requires
// async I/O.
//
// +cache="session"
func (j *Java) Container(
	ctx context.Context,
	source *dagger.Directory,
) (*dagger.Container, error) {
	jdk, err := j.resolveJdkVersion(ctx, source)
	if err != nil {
		return nil, err
	}
	return dag.Container().
		From("eclipse-temurin:"+jdk+"-jdk").
		WithMountedDirectory(workdir, source).
		WithWorkdir(workdir), nil
}

// ToolVersion returns the JDK version banner (`java -version`) for the pinned
// JDK. It is source-less: the version comes from New() or the module-pinned
// LTS default.
//
// +cache="session"
func (j *Java) ToolVersion(ctx context.Context) (string, error) {
	out, err := j.bareContainer().
		WithExec([]string{"sh", "-c", "java -version 2>&1"}).
		Stdout(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Run runs `java -jar <jar> [args...]` against the supplied source and returns
// the program's stdout. jar is the path to the runnable jar within source.
//
// +cache="session"
func (j *Java) Run(
	ctx context.Context,
	source *dagger.Directory,
	jar string,
	// +optional
	args []string,
) (string, error) {
	ctr, err := j.Container(ctx, source)
	if err != nil {
		return "", err
	}
	cmd := []string{"java", "-jar", jar}
	cmd = append(cmd, args...)
	return ctr.WithExec(cmd).Stdout(ctx)
}

// Maven returns a Maven build-tool object bound to source. The JDK pin from
// New() (if any) is propagated; an unpinned Java infers the JDK from source.
// disableWrapper forces the image's system `mvn` even when an in-repo `mvnw`
// is present.
func (j *Java) Maven(
	source *dagger.Directory,
	// +default=false
	disableWrapper bool,
) *Maven {
	return &Maven{
		Source:         source,
		JdkVersion:     j.Version,
		DisableWrapper: disableWrapper,
	}
}

// Gradle returns a Gradle build-tool object bound to source. The JDK pin from
// New() (if any) is propagated; an unpinned Java infers the JDK from source.
// disableWrapper forces the image's system `gradle` even when an in-repo
// `gradlew` is present.
func (j *Java) Gradle(
	source *dagger.Directory,
	// +default=false
	disableWrapper bool,
) *Gradle {
	return &Gradle{
		Source:         source,
		JdkVersion:     j.Version,
		DisableWrapper: disableWrapper,
	}
}

// bareContainer is the source-less JDK container at j.Version (or the
// module-pinned default). Used by ToolVersion.
func (j *Java) bareContainer() *dagger.Container {
	jdk := j.Version
	if jdk == "" {
		jdk = defaultJdkVersion
	}
	return dag.Container().From("eclipse-temurin:" + jdk + "-jdk")
}

// resolveJdkVersion returns j.Version when set; otherwise it infers the JDK
// major version from source in priority order — .java-version, then pom.xml
// (<maven.compiler.release>, then <release>, then <java.version>), then
// build.gradle(.kts) (JavaLanguageVersion.of(...), then sourceCompatibility) —
// falling back to the module-pinned LTS default. Missing/unreadable files are
// skipped rather than erroring.
func (j *Java) resolveJdkVersion(ctx context.Context, source *dagger.Directory) (string, error) {
	if j.Version != "" {
		return j.Version, nil
	}
	return resolveJdkVersionFromSource(ctx, source), nil
}

// resolveJdkVersionFromSource runs the inference cascade against source,
// returning the module-pinned default when nothing is declared.
func resolveJdkVersionFromSource(ctx context.Context, source *dagger.Directory) string {
	if source == nil {
		return defaultJdkVersion
	}
	if c, err := source.File(".java-version").Contents(ctx); err == nil {
		if v := parseJavaVersionFile(c); v != "" {
			return v
		}
	}
	if c, err := source.File("pom.xml").Contents(ctx); err == nil {
		if v := parsePomJavaVersion(c); v != "" {
			return v
		}
	}
	for _, name := range []string{"build.gradle", "build.gradle.kts"} {
		if c, err := source.File(name).Contents(ctx); err == nil {
			if v := parseGradleJavaVersion(c); v != "" {
				return v
			}
		}
	}
	return defaultJdkVersion
}

// majorJava reduces a version string to its Java major number: "1.8" -> "8",
// "17.0.2" -> "17", "21" -> "21", "temurin-21" -> "21". Returns "" when no
// numeric version is found.
func majorJava(v string) string {
	v = strings.TrimSpace(v)
	// Pull the first dotted-numeric run out of the string (handles prefixes
	// like "temurin-" in .java-version files).
	m := regexp.MustCompile(`[0-9]+(?:\.[0-9]+)*`).FindString(v)
	if m == "" {
		return ""
	}
	// Drop a leading legacy "1." (1.8 -> 8); otherwise take the first field.
	if strings.HasPrefix(m, "1.") {
		parts := strings.Split(m, ".")
		if len(parts) >= 2 {
			return parts[1]
		}
	}
	return strings.Split(m, ".")[0]
}

// parseJavaVersionFile reads a .java-version file's contents (e.g. "17" or
// "temurin-21.0.2") and returns the Java major version.
func parseJavaVersionFile(content string) string {
	return majorJava(content)
}

var (
	pomReleaseRe         = regexp.MustCompile(`<maven\.compiler\.release>\s*([0-9.]+)\s*</maven\.compiler\.release>`)
	pomBareReleaseRe     = regexp.MustCompile(`<release>\s*([0-9.]+)\s*</release>`)
	pomJavaVersionRe     = regexp.MustCompile(`<java\.version>\s*([0-9.]+)\s*</java\.version>`)
	gradleLangVersionRe  = regexp.MustCompile(`JavaLanguageVersion\.of\(\s*([0-9]+)\s*\)`)
	gradleSourceCompatRe = regexp.MustCompile(`sourceCompatibility\s*=?\s*(?:JavaVersion\.VERSION_)?['"]?([0-9._]+)`)
)

// parsePomJavaVersion scans a pom.xml for the JDK version in priority order:
// <maven.compiler.release>, then <release>, then <java.version>.
func parsePomJavaVersion(content string) string {
	for _, re := range []*regexp.Regexp{pomReleaseRe, pomBareReleaseRe, pomJavaVersionRe} {
		if m := re.FindStringSubmatch(content); m != nil {
			if v := majorJava(m[1]); v != "" {
				return v
			}
		}
	}
	return ""
}

// parseGradleJavaVersion scans a build.gradle(.kts) for the JDK version,
// preferring the toolchain spelling JavaLanguageVersion.of(N) over
// sourceCompatibility.
func parseGradleJavaVersion(content string) string {
	for _, re := range []*regexp.Regexp{gradleLangVersionRe, gradleSourceCompatRe} {
		if m := re.FindStringSubmatch(content); m != nil {
			// VERSION_1_8 lands as "1_8"; normalise underscores to dots so
			// majorJava can apply its legacy "1.x" rule.
			if v := majorJava(strings.ReplaceAll(m[1], "_", ".")); v != "" {
				return v
			}
		}
	}
	return ""
}
