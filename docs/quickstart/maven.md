# Quick start: a maven repo

A maven repo serves release versions in the Maven 2 layout to `mvn` and Gradle: `mvn deploy` and
Gradle's `publish` put a version in, one file per `PUT`, and the `PUT` of its
`maven-metadata.xml` publishes them together as one version; `mvn dependency:get`, a build's
dependencies and Gradle resolve from it. Every version is one OCI artifact in the registry behind
Lajstrom, with its jar, POM, classifiers and signatures as layers and the four checksums of every
file in its metadata layer. `maven-metadata.xml` and the `.md5`, `.sha1`, `.sha256` and `.sha512`
files are served from the index. SNAPSHOT versions go to a repo of their own, where each deploy
is a build of its own ([below](#snapshot-versions-a-repo-of-builds)). The endpoints are
specified in [http-routing.md](../spec/http-routing.md) §3 (maven); what lands in the registry is
the [manifest profile](../spec/manifest-profile.md). The output shown is that of a run of this
page.

## 1. Start the dev stack, make a repo and a user

You need Go, Docker, `curl` and `jq`; Maven and Gradle run in `maven:3.9.16-eclipse-temurin-21`
and `gradle:8.14.6-jdk21`, the images of `test/e2e/maven.sh`, so nothing of Java is needed on the
host. `crane` (section 7) is optional.

```sh
make dev-up                        # Zot, distribution and Postgres: docs/dev-stack.md
make run                           # serves deploy/compose/lajstrom.example.yaml on 127.0.0.1:8080
```

The steps assume a fresh `lajstrom.db`: after another quick start, stop `make run`, remove
`lajstrom.db` and start it again, or the commands below exit 1 with `user alice already exists`.
In another terminal, once `make run` has logged `listening` (`serve` creates the tables of
`lajstrom.db`; before that, `admin` fails with `no database at …/lajstrom.db`):

```sh
cfg=deploy/compose/lajstrom.example.yaml
bin/lajstrom admin repo create maven/releases --registry local --config $cfg
bin/lajstrom admin user create --name alice --config $cfg     # asks for a password, or takes it with --password-stdin
bin/lajstrom admin binding create --principal user:alice --role writer --mask maven/releases --config $cfg
```

```json
{"format":"maven","name":"releases","registry":"local","kind":"hosted","anonymousRead":false,"policy":null,"createdAt":"2026-11-11T07:27:03.283794Z","updatedAt":"2026-11-11T07:27:03.283794Z"}
{"uid":"01a1c980-ca9b-7277-be2a-20bffb835b38","name":"alice","kind":"local","disabled":false,"createdAt":"2026-11-11T07:27:03.323657Z"}
{"id":1,"principal":"user:01a1c980-ca9b-7277-be2a-20bffb835b38","name":"alice","role":"writer","repoMask":"maven/releases","createdAt":"2026-11-11T07:27:03.338382Z"}
```

The binding holds alice's uid, not her name, so it stays hers when she is renamed
([auth.md](../auth.md#the-uid-and-the-name)). It lets alice deploy to `maven/releases` and
resolve from it: the role `writer` is `read` and `write` on the repos its mask names. Without it
the repo answers her `404`, as a repo that does not exist would; a user who only resolves gets
`reader`. Nothing is open: without
credentials every request is `401`, and Maven and Gradle send theirs when they get it.

## 2. Credentials: a deploy token or a personal access token

A build machine deploys with a deploy token of the repo: it reaches this repo only, and goes in
HTTP Basic as the password of the user name `deploy`. Maven reads `maven-metadata.xml` before
it deploys, so the token needs `read` as well as `write`. Its secret is printed this once:

```sh
bin/lajstrom admin token create --repo maven/releases --name ci --scopes read,write --config $cfg | jq -r .secret > maven-token
```

A person deploys and resolves with their user name and a personal access token, which acts as
the user ([auth.md](../auth.md#tokens)):

```sh
bin/lajstrom admin token create --user alice --name laptop --scopes read,write --config $cfg | jq -r .secret > alice-pat
```

Both files hold a secret, which starts with `ljs_`: keep them out of git, and remove them when
you are done (section 8).

## 3. Maven: settings.xml and the repository

A container on the host's network, where `localhost:8080` is Lajstrom (Linux and OrbStack;
Docker Desktop is not tested, and its `--network host` needs Settings, Resources, Network, Enable
host networking), with the token in its environment:

```sh
docker run --rm -it --network host -e LAJSTROM_TOKEN="$(cat maven-token)" \
  -e JAVA_TOOL_OPTIONS=-Djava.net.preferIPv4Stack=true maven:3.9.16-eclipse-temurin-21 bash
```

`JAVA_TOOL_OPTIONS` is for OrbStack: its `--network host` forwards a connection to the Mac's
`127.0.0.1` from an IPv4 socket only, and Java's sockets are IPv6 by default. Every Java command
prints `Picked up JAVA_TOOL_OPTIONS: -Djava.net.preferIPv4Stack=true` first because of it; on
Linux it can go.

In the container, the credentials go in `~/.m2/settings.xml`, in a `<server>` whose `id` the
repository of the POM, or of the command line, names:

```sh
mkdir -p ~/.m2 && cat > ~/.m2/settings.xml <<'EOF'
<settings>
  <servers>
    <server>
      <id>lajstrom</id>
      <username>deploy</username>
      <password>${env.LAJSTROM_TOKEN}</password>
    </server>
  </servers>
</settings>
EOF
```

With a personal access token, `<username>` is the user's (`alice`) and `<password>` the token.

In a project's `pom.xml`, the repo is where dependencies come from and where `mvn deploy` puts
the project's releases:

```xml
<repositories>
  <repository>
    <id>lajstrom</id>
    <url>http://localhost:8080/maven/releases</url>
    <snapshots><enabled>false</enabled></snapshots>
  </repository>
</repositories>
<distributionManagement>
  <repository>
    <id>lajstrom</id>
    <url>http://localhost:8080/maven/releases</url>
  </repository>
</distributionManagement>
```

Maven 3.8.1 and later refuse a repository over plain HTTP unless it is on `localhost`; anywhere
else Lajstrom is behind TLS and the URL is `https://<host>/maven/releases`.

## 4. Deploy with mvn

A library from the quickstart archetype, at a release version, with its sources jar, deployed
without a change to its POM:

```sh
mvn -B archetype:generate -DarchetypeArtifactId=maven-archetype-quickstart -DarchetypeVersion=1.5 \
  -DgroupId=com.example -DartifactId=lib-core -Dversion=1.2.0
cd lib-core
mvn -B source:jar-no-fork deploy -DaltDeploymentRepository=lajstrom::http://localhost:8080/maven/releases
```

The end of the output, after the build and the tests:

```text
[INFO] --- deploy:3.1.2:deploy (default-deploy) @ lib-core ---
[INFO] Using alternate deployment repository lajstrom::http://localhost:8080/maven/releases
[INFO] Uploading to lajstrom: http://localhost:8080/maven/releases/com/example/lib-core/1.2.0/lib-core-1.2.0.pom
[INFO] Uploaded to lajstrom: http://localhost:8080/maven/releases/com/example/lib-core/1.2.0/lib-core-1.2.0.pom (3.1 kB at 31 kB/s)
[INFO] Uploading to lajstrom: http://localhost:8080/maven/releases/com/example/lib-core/1.2.0/lib-core-1.2.0.jar
[INFO] Uploading to lajstrom: http://localhost:8080/maven/releases/com/example/lib-core/1.2.0/lib-core-1.2.0-sources.jar
[INFO] Uploaded to lajstrom: http://localhost:8080/maven/releases/com/example/lib-core/1.2.0/lib-core-1.2.0.jar (2.5 kB at 39 kB/s)
[INFO] Uploaded to lajstrom: http://localhost:8080/maven/releases/com/example/lib-core/1.2.0/lib-core-1.2.0-sources.jar (2.3 kB at 21 kB/s)
[INFO] Downloading from lajstrom: http://localhost:8080/maven/releases/com/example/lib-core/maven-metadata.xml
[INFO] Uploading to lajstrom: http://localhost:8080/maven/releases/com/example/lib-core/maven-metadata.xml
[INFO] Uploaded to lajstrom: http://localhost:8080/maven/releases/com/example/lib-core/maven-metadata.xml (299 B at 3.4 kB/s)
[INFO] ------------------------------------------------------------------------
[INFO] BUILD SUCCESS
[INFO] ------------------------------------------------------------------------
[INFO] Total time:  7.832 s
[INFO] Finished at: 2026-11-11T07:27:42Z
[INFO] ------------------------------------------------------------------------
```

The jar, the sources jar and the POM are one version: `com.example:lib-core:1.2.0`. Maven also
sends a `.sha1` and a `.md5` of each file, which the server log shows as `PUT`s answered `201`
like the files: the checksum files Maven sends are checked against the files and not stored;
Lajstrom computes all four itself. `maven-metadata.xml` is `404` before the first version, and
its `PUT` is the one that publishes the version. The same version deployed again unchanged is
the idempotent repeat; with other bytes it is `409`: a release is immutable. Earlier in the
output Maven warns that the newest `maven-source-plugin` (a Maven 4 beta) does not fit Maven
3.9 and takes 3.4.0; that is harmless.

### One version from several CI jobs

A version is the files of one deploy, and nothing is added to it afterwards: the `PUT` of
`maven-metadata.xml` commits the files of one publication session, and the version's tag is
final. When several CI jobs build parts of a release, such as native classifiers on runners of
their own platform, they hand their files to one last job, and that job deploys the whole version
once. Gradle's `publish` is the same: one job publishes a version. The modules of a multi-module
build are versions of their own, one per `artifactId`, so separate jobs may deploy separate
modules. What must come from one job is every file of one `groupId:artifactId:version`, the parent
POM included when every job's build would deploy it.

In GitHub Actions, the jobs of a matrix build the classifiers and upload them as workflow
artifacts; the deploy job needs all of them, so it runs only when every leg passed, and one
`mvn deploy` sends the jar, the POM and every classifier in one session. The workflow is an
example, not run by this page:

```yaml
jobs:
  natives:
    strategy:
      matrix:
        include:
          - {os: ubuntu-24.04, classifier: linux-x86_64}
          - {os: ubuntu-24.04-arm, classifier: linux-aarch_64}
          - {os: macos-15, classifier: osx-aarch_64}
          - {os: windows-2025, classifier: windows-x86_64}
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-java@v4
        with: {distribution: temurin, java-version: '21', cache: maven}
      # the project's native build, which writes target/lib-core-<version>-<classifier>.jar
      - run: mvn -B -P native package
      - uses: actions/upload-artifact@v4
        with:
          name: native-${{ matrix.classifier }}
          path: target/*-${{ matrix.classifier }}.jar
          if-no-files-found: error
  deploy:
    needs: natives
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-java@v4
        with:
          distribution: temurin
          java-version: '21'
          server-id: lajstrom              # the <id> of <distributionManagement>
          server-username: LAJSTROM_USER   # the names of the variables below
          server-password: LAJSTROM_TOKEN
      - uses: actions/download-artifact@v4
        with: {pattern: native-*, path: natives, merge-multiple: true}
      - run: mvn -B -P attach-natives deploy
        env:
          LAJSTROM_USER: deploy
          LAJSTROM_TOKEN: ${{ secrets.LAJSTROM_TOKEN }}
```

The profile `attach-natives` of the POM attaches the downloaded files to the build, so that the
deploy sends them with its own:

```xml
<profile>
  <id>attach-natives</id>
  <build>
    <plugins>
      <plugin>
        <groupId>org.codehaus.mojo</groupId>
        <artifactId>build-helper-maven-plugin</artifactId>
        <version>3.6.0</version>
        <executions>
          <execution>
            <phase>package</phase>
            <goals><goal>attach-artifact</goal></goals>
            <configuration>
              <artifacts>
                <artifact>
                  <file>${project.basedir}/natives/${project.artifactId}-${project.version}-linux-x86_64.jar</file>
                  <type>jar</type>
                  <classifier>linux-x86_64</classifier>
                </artifact>
                <!-- one <artifact> for each leg of the matrix -->
              </artifacts>
            </configuration>
          </execution>
        </executions>
      </plugin>
    </plugins>
  </build>
</profile>
```

Without a change to the POM, the deploy job can run `mvn deploy:deploy-file` with
`-DpomFile=pom.xml`, `-Dfile=` the jar, and the classifiers in `-Dfiles=`, `-Dclassifiers=` and
`-Dtypes=`, which also sends one version in one session. Jobs that pass modules to each other
with `mvn install` and an uploaded `~/.m2/repository/com/example` keep the same rule: the last
job deploys, and every version comes from one `mvn deploy`.

What two jobs that each deploy files of one version get today, checked by
`TestDeployTwoJobsOneVersion` in `internal/format/maven` (the file during the commit by
`TestSessionCommitOnce` in `internal/ingest`):

- **One token for both jobs** (one principal): their files go into one session, and the first
  `maven-metadata.xml` of either job commits whatever has arrived from both. That job passes,
  and the other one gets:
  - `200` for its `maven-metadata.xml` when all of its files arrived before the commit: the
    version is whole, by the timing alone;
  - `409` for a file that arrives while the commit runs;
  - `201` for a file after the commit, which opens a new session. A checksum file whose file
    went into the committed session is `400`, which mvn logs before it goes on. Its
    `maven-metadata.xml` then commits the new session: `400` when that session has no POM (mvn
    sends the POM first, so it went into the version), else `409`, other files under a version
    that exists, and the new session is aborted.
- **Two tokens, or a token and a user** (two principals): the second job's first file is `409`
  while the first job's session is open (`open by another principal`), and mvn stops. Its deploy
  after the commit, run again or started late, is `409`.

Either way the version is what one session held: it can lack files of the other job, nothing in
it says so, and it cannot be completed later, since the tag is immutable; the missing files need
a new version. A longer `maven.sessionTimeout` ([api.md](../api.md#policy)) changes how long a
session waits, not which `maven-metadata.xml` commits it. A shared session that several jobs fill
and a last step commits is a design, not built:
[maven-shared-staging-2026-11.md](../design/maven-shared-staging-2026-11.md) (#573).

## 5. Resolve with mvn

Outside the project, into a clean local repository, so that every file comes from Lajstrom (in
`lib-core`'s directory its own POM would stand in for the one in the repo):

```sh
cd ..
mvn -B dependency:get -Dmaven.repo.local=/tmp/m2-clean \
  -DremoteRepositories=lajstrom::default::http://localhost:8080/maven/releases -Dartifact=com.example:lib-core:1.2.0
mvn -B dependency:get -Dmaven.repo.local=/tmp/m2-clean \
  -DremoteRepositories=lajstrom::default::http://localhost:8080/maven/releases -Dartifact=com.example:lib-core:1.2.0:jar:sources
```

Leaving out the lines about Maven Central: the plugins, which Maven downloads into the empty
local repository first; the tries for `lib-core` there, since Maven asks Central before the
repositories of `-DremoteRepositories`; and the JUnit BOM, which `lib-core`'s POM imports:

```text
[INFO] --- dependency:3.7.0:get (default-cli) @ standalone-pom ---
[INFO] Resolving com.example:lib-core:jar:1.2.0 with transitive dependencies
[INFO] Downloading from lajstrom: http://localhost:8080/maven/releases/com/example/lib-core/1.2.0/lib-core-1.2.0.pom
[INFO] Downloaded from lajstrom: http://localhost:8080/maven/releases/com/example/lib-core/1.2.0/lib-core-1.2.0.pom (3.1 kB at 75 kB/s)
[INFO] Downloading from lajstrom: http://localhost:8080/maven/releases/com/example/lib-core/1.2.0/lib-core-1.2.0.jar
[INFO] Downloaded from lajstrom: http://localhost:8080/maven/releases/com/example/lib-core/1.2.0/lib-core-1.2.0.jar (2.5 kB at 364 kB/s)
[INFO] ------------------------------------------------------------------------
[INFO] BUILD SUCCESS
...
[INFO] --- dependency:3.7.0:get (default-cli) @ standalone-pom ---
[INFO] Resolving com.example:lib-core:jar:sources:1.2.0 with transitive dependencies
[INFO] Downloading from lajstrom: http://localhost:8080/maven/releases/com/example/lib-core/1.2.0/lib-core-1.2.0-sources.jar
[INFO] Downloaded from lajstrom: http://localhost:8080/maven/releases/com/example/lib-core/1.2.0/lib-core-1.2.0-sources.jar (2.3 kB at 116 kB/s)
[INFO] ------------------------------------------------------------------------
[INFO] BUILD SUCCESS
```

Maven checks each download against its `.sha1` from the repo, which it keeps beside the file in
`/tmp/m2-clean/com/example/lib-core/1.2.0/`.

A dependency on a version range is resolved through the artifact's `maven-metadata.xml`, which
lists its versions in Maven's order. A second project, with the `<repositories>` of section 3
(anywhere in `<project>`, before `<build>` for example) and this dependency in the project's
`<dependencies>`, the one after `<dependencyManagement>`, not the one inside it:

```sh
mvn -B archetype:generate -DarchetypeArtifactId=maven-archetype-quickstart -DarchetypeVersion=1.5 \
  -DgroupId=com.example -DartifactId=app -Dversion=1.0.0 && cd app
```

```xml
<dependency>
  <groupId>com.example</groupId>
  <artifactId>lib-core</artifactId>
  <version>[1.0,2.0)</version>
</dependency>
```

```sh
mvn -B dependency:tree -Dmaven.repo.local=/tmp/m2-clean
```

```text
[INFO] --------------------------< com.example:app >---------------------------
[INFO] Building app 1.0.0
[INFO]   from pom.xml
[INFO] --------------------------------[ jar ]---------------------------------
[INFO] Downloading from lajstrom: http://localhost:8080/maven/releases/com/example/lib-core/maven-metadata.xml
[INFO] Downloaded from lajstrom: http://localhost:8080/maven/releases/com/example/lib-core/maven-metadata.xml (326 B at 4.6 kB/s)
[INFO] Downloading from lajstrom: http://localhost:8080/maven/releases/org/junit/jupiter/junit-jupiter-api/5.11.0/junit-jupiter-api-5.11.0.pom
[INFO] Downloading from lajstrom: http://localhost:8080/maven/releases/org/opentest4j/opentest4j/1.3.0/opentest4j-1.3.0.pom
[INFO] Downloading from lajstrom: http://localhost:8080/maven/releases/org/junit/platform/junit-platform-commons/1.11.0/junit-platform-commons-1.11.0.pom
[INFO] Downloading from lajstrom: http://localhost:8080/maven/releases/org/apiguardian/apiguardian-api/1.1.2/apiguardian-api-1.1.2.pom
[INFO] Downloading from lajstrom: http://localhost:8080/maven/releases/org/junit/jupiter/junit-jupiter-params/5.11.0/junit-jupiter-params-5.11.0.pom
[INFO] 
[INFO] --- dependency:3.7.0:tree (default-cli) @ app ---
[INFO] com.example:app:jar:1.0.0
[INFO] +- com.example:lib-core:jar:1.2.0:compile
[INFO] +- org.junit.jupiter:junit-jupiter-api:jar:5.11.0:test
[INFO] |  +- org.opentest4j:opentest4j:jar:1.3.0:test
[INFO] |  +- org.junit.platform:junit-platform-commons:jar:1.11.0:test
[INFO] |  \- org.apiguardian:apiguardian-api:jar:1.1.2:test
[INFO] \- org.junit.jupiter:junit-jupiter-params:jar:5.11.0:test
[INFO] ------------------------------------------------------------------------
[INFO] BUILD SUCCESS
```

The range resolved to `1.2.0`, the only version in `maven-metadata.xml`; its POM is not
downloaded again, `dependency:get` put it into `/tmp/m2-clean`. A repository of the
POM comes before Maven Central, so Maven asks Lajstrom first for every dependency of the
project, the JUnit test dependencies too: they are not there (`404`) and come from Maven
Central, in the lines left out.

## 6. Gradle

A library `lib-extra` built with Gradle, which depends on `lib-core` through a range: Gradle
resolves `lib-core` from the repo and publishes `lib-extra` to it. On the host, in the directory
of `maven-token` (the repository's root, after section 2):

```sh
mkdir -p lib-extra/src/main/java/com/example/extra && cd lib-extra
cat > src/main/java/com/example/extra/Extra.java <<'EOF'
package com.example.extra;

public class Extra {
    public static void main(String[] args) {
        com.example.App.main(args);
    }
}
EOF
```

In `settings.gradle.kts` (or the `repositories` of `build.gradle.kts`), the repo, with the
credentials as the Gradle properties `lajstromUsername` and `lajstromPassword` (in
`~/.gradle/gradle.properties`, or `ORG_GRADLE_PROJECT_lajstromUsername` and
`ORG_GRADLE_PROJECT_lajstromPassword` in the environment), and Maven Central after it for what
is not in Lajstrom: `lib-core`'s POM imports the JUnit BOM, which Gradle reads to resolve it.

```kotlin
rootProject.name = "lib-extra"

dependencyResolutionManagement {
    repositories {
        maven {
            name = "lajstrom"
            url = uri("http://localhost:8080/maven/releases")
            isAllowInsecureProtocol = true // the dev stack's plain HTTP only
            credentials(PasswordCredentials::class)
        }
        mavenCentral()
    }
}
```

Publishing, in `build.gradle.kts`, with the `java-library` and `maven-publish` plugins:

```kotlin
plugins {
    `java-library`
    `maven-publish`
}

group = "com.example"
version = "1.0.0"

dependencies {
    api("com.example:lib-core:[1.0,2.0)")
}

publishing {
    publications {
        create<MavenPublication>("lib") {
            artifactId = "lib-extra"
            from(components["java"])
        }
    }
    repositories {
        maven {
            name = "lajstrom"
            url = uri("http://localhost:8080/maven/releases")
            isAllowInsecureProtocol = true
            credentials(PasswordCredentials::class)
        }
    }
}
```

```sh
docker run --rm --network host -v "$PWD:/w" -w /w -e JAVA_TOOL_OPTIONS=-Djava.net.preferIPv4Stack=true \
  -e ORG_GRADLE_PROJECT_lajstromUsername=deploy -e ORG_GRADLE_PROJECT_lajstromPassword="$(cat ../maven-token)" \
  gradle:8.14.6-jdk21 gradle publish dependencies --configuration runtimeClasspath
```

```text
Picked up JAVA_TOOL_OPTIONS: -Djava.net.preferIPv4Stack=true
Starting a Gradle Daemon (subsequent builds will be faster)
> Task :compileJava
> Task :processResources NO-SOURCE
> Task :classes
> Task :jar
> Task :generateMetadataFileForLibPublication
> Task :generatePomFileForLibPublication
> Task :publishLibPublicationToLajstromRepository
> Task :publish

> Task :dependencies

------------------------------------------------------------
Root project 'lib-extra'
------------------------------------------------------------

runtimeClasspath - Runtime classpath of source set 'main'.
\--- com.example:lib-core:[1.0,2.0) -> 1.2.0

A web-based, searchable dependency report is available by adding the --scan option.

BUILD SUCCESSFUL in 14s
6 actionable tasks: 6 executed
```

Gradle read `lib-core`'s `maven-metadata.xml` for the range, took the POM and the jar of `1.2.0`
from the repo, and asked it for the JUnit BOM before Maven Central (`404`). It published the jar,
the POM and its module metadata (`lib-extra-1.0.0.module`), each with its `.md5`, `.sha1`,
`.sha256` and `.sha512`, then `maven-metadata.xml`: twenty `PUT`s, each `201` in the server's
log, and one version, `com.example:lib-extra:1.0.0`, of three files. Gradle may check a download
against its `.sha512`, `.sha256` or `.sha1`: all four are there for every file.

## 7. By hand, and in the registry

The files are plain `GET`s, and `HEAD` answers the same headers. Back in the directory of
`maven-token`:

```sh
cd ..
t="deploy:$(cat maven-token)"
curl -s -u "$t" http://localhost:8080/maven/releases/com/example/lib-core/maven-metadata.xml
curl -s -u "$t" http://localhost:8080/maven/releases/com/example/lib-core/maven-metadata.xml.sha1
```

```text
<?xml version="1.0" encoding="UTF-8"?>
<metadata>
  <groupId>com.example</groupId>
  <artifactId>lib-core</artifactId>
  <versioning>
    <latest>1.2.0</latest>
    <release>1.2.0</release>
    <versions>
      <version>1.2.0</version>
    </versions>
    <lastUpdated>20261111072742</lastUpdated>
  </versioning>
</metadata>
dfb3ee6937e1ed465b2e39c93332dae1e840c2e4
```

`maven-metadata.xml` lists the versions in Maven's order; `latest` is the greatest of them and
`release` the greatest that is not a pre-release (an alpha, beta, milestone or rc), and
`lastUpdated` is when the newest version was published (the `Finished at` of section 4). It is
rendered from the index on the first `GET` after a deploy and kept until the next, and answers
`If-None-Match` with `304`.

A file and its checksum, the same as `sha512sum lib-core-1.2.0.jar` (`shasum -a 512` on macOS):

```sh
curl -s -u "$t" -o lib-core-1.2.0.jar http://localhost:8080/maven/releases/com/example/lib-core/1.2.0/lib-core-1.2.0.jar
curl -s -u "$t" http://localhost:8080/maven/releases/com/example/lib-core/1.2.0/lib-core-1.2.0.jar.sha512
```

```text
6023165726facf779f5c87601866e80d90b35f811f0caa8cfbaca2890efa484b8fdcb335e8e3be71e3da856619e9c0b77e58a703747723e094f5c37b90c96b9e
```

Your run gives other hashes: the jar holds the time it was built, and `maven-metadata.xml` the
time of the deploy.

```sh
curl -sI -u "$t" http://localhost:8080/maven/releases/com/example/lib-core/1.2.0/lib-core-1.2.0.pom
```

```text
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 3092
Content-Security-Policy: default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'
Content-Type: application/xml
Docker-Content-Digest: sha256:a870ceeb1afdf35695dfe08b437f15504ff3346bb7c407b7950b5c908489d6a0
Etag: "sha256:99496f5c1a0f56d6297a0780f5ce434528b34f3d17384168b7d22516bde276f1"
Last-Modified: Wed, 11 Nov 2026 07:27:42 GMT
Referrer-Policy: no-referrer
X-Content-Type-Options: nosniff
X-Request-Id: 2158586bf297f6c5b4f4b2ca468f6b12
Date: Wed, 11 Nov 2026 07:30:26 GMT
```

`Docker-Content-Digest` is the digest of the version's manifest, `Etag` the sha256 of the file.
Each version is a manifest under `maven/releases/<groupId lowercased>/<artifactId>`, tagged with
the version ([the tag scheme](../spec/manifest-profile.md)):

```sh
crane ls localhost:5000/maven/releases/com.example/lib-core
crane manifest localhost:5000/maven/releases/com.example/lib-core:1.2.0 | jq '.annotations, [.layers[].annotations]'
```

```text
1.2.0
{
  "lajstrom.format": "maven",
  "lajstrom.maven.artifactId": "lib-core",
  "lajstrom.maven.groupId": "com.example",
  "lajstrom.maven.packaging": "jar",
  "lajstrom.name": "com.example:lib-core",
  "lajstrom.repo": "releases",
  "lajstrom.schema": "1",
  "lajstrom.uploaded-by": "token:1",
  "lajstrom.version": "1.2.0",
  "org.opencontainers.image.created": "2026-11-11T07:27:42Z"
}
[
  {
    "lajstrom.maven.classifier": "sources",
    "lajstrom.maven.extension": "jar",
    "org.opencontainers.image.title": "lib-core-1.2.0-sources.jar"
  },
  {
    "lajstrom.maven.extension": "jar",
    "org.opencontainers.image.title": "lib-core-1.2.0.jar"
  },
  {
    "org.opencontainers.image.title": "lib-core-1.2.0.pom"
  },
  {
    "org.opencontainers.image.title": "metadata.json"
  }
]
```

`lajstrom.uploaded-by` is the deploy token of section 2 (`token:1`, its id). The checksums are
not layers: they are in the metadata layer, last.

`bin/lajstrom reindex --repo maven/releases --config $cfg` rebuilds the index of the repo from
those manifests ([operations.md](../operations.md#reindex)).

## 8. Clean up

Leave the Maven container (`exit`) and stop `make run` (Ctrl-C) first: on its way out the server
folds `lajstrom.db-wal` back into `lajstrom.db`, so one file is left to remove.

```sh
rm maven-token alice-pat lib-core-1.2.0.jar   # the tokens are secrets
rm -rf lib-extra                               # sudo on Linux: the gradle image wrote its build as root
make dev-down RESET=1                          # the registries forget what they hold
rm lajstrom.db                                 # the index, with the users and tokens
```

`bin/lajstrom admin token revoke <id> --config $cfg` ends a token without the rest
(`admin token list --repo maven/releases` shows the ids).

## SNAPSHOT versions: a repo of builds

A repo takes release versions or SNAPSHOT versions, not both, as Maven repositories do: the
releases of this page stay in `maven/releases`, and the SNAPSHOT versions get a repo whose
policy says so ([api.md](../api.md#policy)). Each `mvn deploy` of `1.3.0-SNAPSHOT` is a build of
its own, its files named with the time of the deploy and a build number,
`lib-core-1.3.0-20261114.101112-1.jar`, then `…-2.jar`: a version with a tag of its own, never
changed afterwards. `1.3.0-SNAPSHOT` itself is never a tag. What it resolves to, the latest build,
is what `1.3.0-SNAPSHOT/maven-metadata.xml` says, rendered from the builds the repo has. This
section shows the commands without their output; `test/e2e/maven.sh` runs the same deploys and
checks them.

```sh
bin/lajstrom admin repo create maven/snapshots --registry local --policy '{"maven":{"versionPolicy":"snapshot"}}' --config $cfg
bin/lajstrom admin binding create --principal user:alice --role writer --mask maven/snapshots --config $cfg
bin/lajstrom admin token create --repo maven/snapshots --name ci-snapshots --scopes read,write --config $cfg | jq -r .secret > maven-snapshots-token
```

In `settings.xml` a second `<server>`, `lajstrom-snapshots`, with that token (or alice's personal
access token, which her binding lets into both repos), and in the POM the snapshot repository
beside the release one:

```xml
<repositories>
  <repository>
    <id>lajstrom</id>
    <url>http://localhost:8080/maven/releases</url>
    <snapshots><enabled>false</enabled></snapshots>
  </repository>
  <repository>
    <id>lajstrom-snapshots</id>
    <url>http://localhost:8080/maven/snapshots</url>
    <releases><enabled>false</enabled></releases>
  </repository>
</repositories>
<distributionManagement>
  <repository>
    <id>lajstrom</id>
    <url>http://localhost:8080/maven/releases</url>
  </repository>
  <snapshotRepository>
    <id>lajstrom-snapshots</id>
    <url>http://localhost:8080/maven/snapshots</url>
  </snapshotRepository>
</distributionManagement>
```

`mvn deploy` of a project at `1.3.0-SNAPSHOT` reads `1.3.0-SNAPSHOT/maven-metadata.xml` first
(`404` before the first build), takes the next build number from it, sends the files of the build,
then that document, which publishes the build (`201`), and the artifact's `maven-metadata.xml`
last (`200`: nothing left to publish). Deployed again, the next build:

```sh
mvn -B deploy      # in a project whose version is 1.3.0-SNAPSHOT: the build 1.3.0-<timestamp>-1
mvn -B deploy      # the build 1.3.0-<timestamp>-2; the first one stays as it was
curl -s -u "deploy:$(cat maven-snapshots-token)" http://localhost:8080/maven/snapshots/com/example/lib-core/1.3.0-SNAPSHOT/maven-metadata.xml
```

The document names the latest build in `snapshot` (its `timestamp` and `buildNumber`) and, for
each classifier and extension, the newest build that has such a file in `snapshotVersions`, as
Maven writes it itself. A build that depends on `1.3.0-SNAPSHOT` gets the latest build when it
checks the repository: once a day by default (`updatePolicy` of the repository), at once with
`mvn -U`. `lib-core-1.3.0-SNAPSHOT.jar` in that directory, the name Maven 2 wrote, is the jar of
the latest build, for a `curl` or a client without the document; a deploy under that name is
`400`, since a build is always named with its timestamp.

With Gradle, `version = "1.3.0-SNAPSHOT"` publishes builds the same way; the repository of
`publishing` is the snapshot repo for such a version:

```kotlin
url = uri(if (version.toString().endsWith("-SNAPSHOT")) "http://localhost:8080/maven/snapshots"
          else "http://localhost:8080/maven/releases")
```

A release deployed to `maven/snapshots`, or a SNAPSHOT version to `maven/releases`, is `400`
naming `policy.maven.versionPolicy`, at the first file and before anything is stored. Builds stay
until they are deleted, through the API as every version is; a scheduled
`lajstrom admin retention --repo maven/snapshots --keep-builds 3` keeps the three newest builds of
each SNAPSHOT version ([operations.md](../operations.md#retention)).

## The end-to-end script

`test/e2e/maven.sh` runs the same clients in the same images with checks: `mvn deploy` of two
versions, `dependency:get`, a range, the idempotent repeat and the `409`, Gradle's `publish` and
a Gradle build that resolves, and two deploys of a SNAPSHOT version to a repo of builds, which a
`dependency:get -U` follows. It makes its repos and deploy tokens and removes them at the end. With the dev stack up, `make e2e` serves a Lajstrom of its own on a free port, with an
admin for the run, and reads the statuses of the clients' requests from its log:

```sh
make e2e E2E=maven                 # PASS maven.sh, in about a minute once the plugins are cached
```

Against a Lajstrom that is running, the script takes the base URL and an admin
([operations.md](../operations.md#the-first-admin)). This page made no admin; against `make run`
the first line makes one, with a password of your own (8 bytes at least):

```sh
pw='a password of your own'
printf '%s' "$pw" | bin/lajstrom admin user create --name admin --admin --password-stdin --config $cfg
LAJSTROM_E2E_USER=admin LAJSTROM_E2E_PASSWORD="$pw" test/e2e/maven.sh http://localhost:8080
```

It keeps the plugins it downloads in the Docker volumes `lajstrom-e2e-m2` and
`lajstrom-e2e-gradle` for the next run; `docker volume rm` removes them.
