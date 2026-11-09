# Quick start: a maven repo

A maven repo serves release versions in the Maven 2 layout to `mvn` and Gradle: `mvn deploy` and
Gradle's `publish` put a version in, one file per `PUT`, and the `PUT` of its
`maven-metadata.xml` publishes them together as one version; `mvn dependency:get`, a build's
dependencies and Gradle resolve from it. Every version is one OCI artifact in the registry behind
Lajstrom, with its jar, POM, classifiers and signatures as layers and the four checksums of every
file in its metadata layer. `maven-metadata.xml` and the `.md5`, `.sha1`, `.sha256` and `.sha512`
files are served from the index. SNAPSHOT versions come after the MVP. The endpoints are
specified in [http-routing.md](../spec/http-routing.md) §3 (maven); what lands in the registry is
the [manifest profile](../spec/manifest-profile.md).

## 1. Start the dev stack, make a repo and a user

You need Go, Docker, `curl` and `jq`; Maven and Gradle run in `maven:3.9-eclipse-temurin-21` and
`gradle:8-jdk21`, so nothing of Java is needed on the host.

```sh
make dev-up                        # Zot, distribution and Postgres: docs/dev-stack.md
make run                           # serves deploy/compose/lajstrom.example.yaml on :8080
```

In another terminal:

```sh
cfg=deploy/compose/lajstrom.example.yaml
bin/lajstrom admin repo create maven/releases --registry local --config $cfg
bin/lajstrom admin user create --name alice --config $cfg     # asks for a password
bin/lajstrom admin binding create --principal user:alice --role writer --mask maven/releases --config $cfg
```

The binding lets alice deploy to `maven/releases` and resolve from it: the role `writer` is
`read` and `write` on the repos its mask names. Without it the repo answers her `404`, as a repo
that does not exist would; a user who only resolves gets `reader`. Nothing is open: without
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

## 3. Maven: settings.xml and the repository

A container on the host's network, where `localhost:8080` is Lajstrom (Linux and OrbStack), with
the token in its environment:

```sh
docker run --rm -it --network host -e LAJSTROM_TOKEN="$(cat maven-token)" maven:3.9-eclipse-temurin-21 bash
```

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

The jar, the sources jar and the POM are one version: `com.example:lib-core:1.2.0`. The checksum
files Maven sends are checked against the files and not stored; Lajstrom computes all four
itself. The same version deployed again unchanged is the idempotent repeat; with other bytes it
is `409`: a release is immutable.

## 5. Resolve with mvn

Into a clean local repository, so that every file comes from Lajstrom:

```sh
mvn -B dependency:get -Dmaven.repo.local=/tmp/m2-clean \
  -DremoteRepositories=lajstrom::default::http://localhost:8080/maven/releases -Dartifact=com.example:lib-core:1.2.0
mvn -B dependency:get -Dmaven.repo.local=/tmp/m2-clean \
  -DremoteRepositories=lajstrom::default::http://localhost:8080/maven/releases -Dartifact=com.example:lib-core:1.2.0:jar:sources
```

A dependency on a version range is resolved through the artifact's `maven-metadata.xml`, which
lists its versions in Maven's order. In a second project, with the `<repositories>` of section 3
and this in its `<dependencies>`:

```sh
cd .. && mvn -B archetype:generate -DarchetypeArtifactId=maven-archetype-quickstart -DarchetypeVersion=1.5   -DgroupId=com.example -DartifactId=app -Dversion=1.0.0 && cd app
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

## 6. Gradle

In `settings.gradle.kts` (or the `repositories` of `build.gradle.kts`), with the credentials as
the Gradle properties `lajstromUsername` and `lajstromPassword` (in `~/.gradle/gradle.properties`,
or `ORG_GRADLE_PROJECT_lajstromUsername` and `ORG_GRADLE_PROJECT_lajstromPassword` in the
environment):

```kotlin
dependencyResolutionManagement {
    repositories {
        maven {
            name = "lajstrom"
            url = uri("http://localhost:8080/maven/releases")
            isAllowInsecureProtocol = true // the dev stack's plain HTTP only
            credentials(PasswordCredentials::class)
        }
    }
}
```

Publishing, in the `build.gradle.kts` of a library with the `java-library` and `maven-publish`
plugins:

```kotlin
group = "com.example"
version = "1.3.0"

publishing {
    publications {
        create<MavenPublication>("lib") {
            artifactId = "lib-core"
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
docker run --rm -it --network host -v "$PWD:/w" -w /w \
  -e ORG_GRADLE_PROJECT_lajstromUsername=deploy -e ORG_GRADLE_PROJECT_lajstromPassword="$(cat maven-token)" \
  gradle:8-jdk21 gradle publish dependencies --configuration runtimeClasspath
```

Gradle publishes its module metadata (`lib-core-1.3.0.module`) beside the POM, as a file of the
version, and may check a download against its `.sha512`, `.sha256` or `.sha1`: all four are there
for every file.

## 7. By hand, and in the registry

The files are plain `GET`s, and `HEAD` answers the same headers:

```sh
t="deploy:$(cat maven-token)"
curl -s -u "$t" http://localhost:8080/maven/releases/com/example/lib-core/maven-metadata.xml
curl -s -u "$t" http://localhost:8080/maven/releases/com/example/lib-core/maven-metadata.xml.sha1
curl -s -u "$t" -o lib-core-1.2.0.jar http://localhost:8080/maven/releases/com/example/lib-core/1.2.0/lib-core-1.2.0.jar
curl -s -u "$t" http://localhost:8080/maven/releases/com/example/lib-core/1.2.0/lib-core-1.2.0.jar.sha512
curl -sI -u "$t" http://localhost:8080/maven/releases/com/example/lib-core/1.2.0/lib-core-1.2.0.pom
```

`maven-metadata.xml` lists the versions in Maven's order; `latest` is the greatest of them and
`release` the greatest that is not a pre-release (an alpha, beta, milestone or rc), and
`lastUpdated` is when the newest version was published. It is rendered from the index on the
first `GET` after a deploy and kept until the next, and answers `If-None-Match` with `304`.

Each version is a manifest under `maven/releases/<groupId lowercased>/<artifactId>`, tagged with
the version ([the tag scheme](../spec/manifest-profile.md)):

```sh
crane ls localhost:5000/maven/releases/com.example/lib-core
crane manifest localhost:5000/maven/releases/com.example/lib-core:1.2.0 | jq '.annotations, [.layers[].annotations]'
```

`lajstrom reindex --repo maven/releases` rebuilds the index of the repo from those manifests
([operations.md](../operations.md#reindex)).
