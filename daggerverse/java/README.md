# java

A Dagger module wrapping the JVM toolchain — the JDK plus the Maven and Gradle
build tools — so downstream pipelines can compile, test, and package Java
projects without re-inventing JDK pinning, build-tool selection, and cache
plumbing.

It follows the `go`/`zig` toolchain shape (a root struct pinned to a version,
a `Container` escape hatch, and `+cache="session"` helpers) and adds two
cooperating build-tool objects (`Maven`, `Gradle`) in the spirit of the
`kafka` `Cluster`/`Client` split. Source is mounted at `/work` and used as the
working directory.

- JDK base image: `eclipse-temurin:<jdk>-jdk`.
- Maven base image: `maven:<ver>-eclipse-temurin-<jdk>`, with `~/.m2/repository`
  mounted as the shared `maven-repository-cache` cache volume.
- Gradle base image: `gradle:<ver>-jdk<jdk>`, with `~/.gradle/caches` mounted as
  the shared `gradle-caches-cache` cache volume. Every Gradle invocation passes
  `--no-daemon` for reproducibility.

## JDK version

`New(version)` pins the JDK major version (e.g. `"21"`). When called with `""`,
the version is inferred from the supplied source in priority order:

1. `.java-version`
2. `pom.xml` — `<maven.compiler.release>`, then `<release>`, then `<java.version>`
3. `build.gradle(.kts)` — `JavaLanguageVersion.of(...)`, then `sourceCompatibility`

If nothing is declared, it falls back to the module-pinned LTS default (`21`).

## Build-tool version

The build-tool version is **not** taken from `New()`. When an in-repo wrapper
(`mvnw`/`gradlew`) is present it is used — pinning the tool per-repo, mirroring
go's `toolchain` directive. `disableWrapper` forces the image's system `mvn`/
`gradle` (whose version is the module-pinned default image tag).

## Function surface

### Root `Java`

| Name | Purpose |
|---|---|
| `Container(source)` | Prepared JDK container at the resolved JDK — escape hatch when a JVM command isn't covered by the typed helpers. |
| `ToolVersion()` | `java -version` for the pinned JDK (source-less). |
| `Run(source, jar, args)` | `java -jar <jar> args...`; returns stdout. |
| `Maven(source, disableWrapper)` | Returns a `Maven` build-tool object. |
| `Gradle(source, disableWrapper)` | Returns a `Gradle` build-tool object. |

### `Maven`

| Name | Purpose |
|---|---|
| `Container()` | Prepared Maven container. |
| `Goals(goals, args)` | Run arbitrary Maven goals; returns stdout. |
| `Compile()` | `mvn compile`; returns the `target` directory. |
| `Test()` | `mvn test`; returns stdout. |
| `Package(skipTests)` | `mvn package` (`-DskipTests` when set); returns `target`. |
| `Verify()` | `mvn verify`; returns stdout. |

### `Gradle`

| Name | Purpose |
|---|---|
| `Container()` | Prepared Gradle container. |
| `Tasks(tasks, args)` | Run arbitrary Gradle tasks (always `--no-daemon`); returns stdout. |
| `Build()` | `gradle build`; returns the `build` directory. |
| `Test()` | `gradle test`; returns stdout. |
| `Assemble()` | `gradle assemble`; returns `build/libs`. |

## CLI quick reference

```sh
# List functions
dagger -m daggerverse/java functions

# Print the pinned JDK version
dagger -m daggerverse/java call --version=21 tool-version

# Package a Maven project (jar lands in the returned target directory)
dagger -m daggerverse/java call maven --source=path/to/project \
    package export --path=./target

# Run the Gradle build for a project
dagger -m daggerverse/java call gradle --source=path/to/project test
```

## Java SDK quick reference

```go
j := dag.Java() // or dag.Java(dagger.JavaOpts{Version: "21"})

// Maven: compile, test, package.
target := j.Maven(src).Package(dagger.JavaMavenPackageOpts{SkipTests: false})

// Gradle: assemble produces build/libs.
libs := j.Gradle(src).Assemble()

// Run a built jar.
out, err := j.Run(ctx, target, "app-1.0.jar")
```

See `tests/main.go` for one example per function.
