<!-- The survey behind ADR 0014, made on 2026-11-10 from public indexes and HTTP headers; kept as it was written, for the record. -->

# Package file sizes per ecosystem: should Lajstrom stop spooling uploads?

Survey made on 2026-11-10 for the decision whether package uploads (raw, apt/.deb, PyPI wheel/sdist,
Maven, npm) should stop spooling to a temporary file and stream straight to the registry while
parsing on the fly. Everything below was measured from public indexes and HTTP headers on that
day; no number is from memory. Sizes are binary (KiB, MiB, GiB = 2^10, 2^20, 2^30 bytes);
"> 1 GiB" means strictly larger than 1,073,741,824 bytes.

## Summary

| population | count | p50 | p90 | p99 | p99.9 | max | > 100 MiB | > 1 GiB | > 4 GiB |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| deb: Debian 13 trixie amd64, all components (full) | 69,913 | 57.5 KiB | 1.6 MiB | 23.7 MiB | 215 MiB | 3.0 GiB | 162 (0.23%) | 3 (0.004%) | 0 |
| deb: Ubuntu 24.04 noble amd64, all components (full) | 72,500 | 54.3 KiB | 2.3 MiB | 35.6 MiB | 186 MiB | 2.7 GiB | 180 (0.25%) | 4 (0.006%) | 0 |
| deb: Debian trixie-debug -dbgsym (full) | 21,085 | 253 KiB | 7.8 MiB | 113 MiB | 492 MiB | 1.2 GiB | 242 (1.15%) | 2 (0.009%) | 0 |
| pypi: wheels, top-5000 latest release | 33,773 | 672 KiB | 8.6 MiB | 87.7 MiB | 388 MiB | 690 MiB | 265 (0.78%) | 0 | 0 |
| pypi: sdists, top-5000 latest release | 4,719 | 80.6 KiB | 1.9 MiB | 32.0 MiB | 99.5 MiB | 435 MiB | 4 (0.085%) | 0 | 0 |
| maven: main jars of Spring Boot + Quarkus BOMs | 3,763 | 31.3 KiB | 689 KiB | 17.5 MiB | 200 MiB | 242 MiB | 9 (0.24%) | 0 | 0 |
| maven: main artifact of 3,000 random 2026 GAs | 2,735 | 39.6 KiB | 839 KiB | 27.8 MiB | 222 MiB | 288 MiB | 7 (0.26%) | 0 | 0 |
| maven: every classified file, 2026-09/10 index | 1,137,690 | 20.1 KiB | 1.0 MiB | 9.1 MiB | 101 MiB | 941 MiB | 1,154 (0.10%) | 0 | 0 |
| npm: tarballs, top-5000 by downloads | 4,996 | 8.1 KiB | 201 KiB | 6.3 MiB | 41.6 MiB | 152 MiB | 2 (0.040%) | 0 | 0 |
| raw: GitHub release assets, 50 big-asset projects | 942 | 37.0 MiB | 347 MiB | 1.4 GiB | 2.0 GiB | 2.0 GiB | 284 (30.15%) | 22 (2.34%) | 0 |

What this means:

- **The answer depends on the tail, not the median.** In every package ecosystem the median file
  is 8–700 KiB and p99 is 6–113 MiB. The largest files are 10 to 100 times the p99, and they set
  both the size of the temp disk and the maxFileSize.
- **.deb is the format with the largest files.** The Debian and Ubuntu archives top out at
  3.0 GiB (`qgis-api-doc`). Ubuntu's kernel `-dbgsym` .ddebs are about 2.0 GiB each, and the
  lead's own builds reach 5 GiB. No public .deb is over 4 GiB.
- **PyPI is small, except for ML.** No pypi.org file is over 1 GiB, either in the top-5000's
  latest releases or in any release of 24 ML giants. The largest is 1000 MiB
  (`libcugraph-cu12`). torch is 514 MiB (all-time 873 MiB),
  `nvidia-cudnn-cu12` is 683–817 MiB, tensorflow is 546 MiB (all-time 615 MiB), and jaxlib is
  ≤ 100 MiB. PyTorch's own index (`download.pytorch.org`), which internal mirrors also host, has
  `+cu118`/`+cu121` wheels of 2.05–2.11 GiB.
- **A 5 GB jar is indeed not a thing.** No jar is over 1 GiB in the ~6 weeks of 2026 Central
  uploads (1.14 M classified files) or in the ~5,600 popular and random jars probed by HEAD.
  The largest jars found on Central are 2.29 GiB (DJL `pytorch-native-cu124`, 2024-11) and 1.9 GiB (bytedeco CUDA
  redist, 2024). The largest plain main jars are 200–290 MiB (shaded "connector"/"app" jars).
- **npm is tiny.** In the top-5000 the p99 is 6.3 MiB and the max is 152 MiB (the
  single-binary platform packages: `@anthropic-ai/claude-code-linux-x64`, `onnxruntime-node`).
- **Raw/generic has no natural ceiling.** In the latest releases of 50 big-asset GitHub projects,
  p50 is 37 MiB, and the largest assets sit right at GitHub's 2 GiB per-file cap.

Answers:

- **(a) Temp-file budget.** At p99 the spool is small: 8 concurrent uploads need under 1 GiB for
  every format except raw. At the observed max it is N × 1–5 GiB. Since a client chooses its own
  size, a safe spool must hold N × maxFileSize. For 8 concurrent uploads that is 40 GiB with
  5 GiB debs and 16 GiB with 2 GiB raw. A spool sized for p99 needs admission control: reserve
  the declared `Content-Length` against a per-replica budget and answer 503 + `Retry-After` when it is full.
- **(b) Streaming with parse is realistic for every format.**
  - Maven, raw: nothing in the body is parsed.
  - .deb: the metadata ends in the first ≤ 0.9 MiB.
  - sdist: already parsed as a stream.
  - npm: the tarball is already read whole as a stream.
  - Wheels are the one format that needs new code. Go's `archive/zip` needs `io.ReaderAt`, and
    METADATA is usually at the very end: at 99.65–99.999 % in the largest wheels, in the last 7
    entries of 70 of 80 random ones. tensorflow is the exception and puts it first.
  - The cost of streaming is shared by all formats: for sdists, wheels and npm the verdict often
    comes only at EOF. A rejected upload then leaves an orphan blob in the registry.
- **(c) maxFileSize defaults** (power-of-two headroom over the observed max):

  | format | default |
  |---|---|
  | npm | 512 MiB |
  | pypi | 4 GiB (1 GiB if only pypi.org-style content is expected) |
  | maven | 4 GiB |
  | deb | 8 GiB (under the ar member limit of 9.31 GiB) |
  | raw | 8 GiB |

  Details in section 8.

## 1. Method and sources

| ecosystem | source (fetched 2026-11-10) | population / sample |
|---|---|---|
| Debian | `deb.debian.org/debian/dists/trixie/{main,contrib,non-free,non-free-firmware}/binary-amd64/Packages.xz` (trixie = 13.7, Release dated 2026-09-12), `Size:` field; plus `debian-debug/dists/trixie-debug/main` | full population: 69,913 .debs (amd64 + arch:all); 21,085 dbgsym |
| Ubuntu | `archive.ubuntu.com/ubuntu/dists/noble/{main,universe,restricted,multiverse}/binary-amd64/Packages.xz` (24.04 release pocket, 2024-04-25), same for `noble-updates`; `ddebs.ubuntu.com` noble + noble-updates main | full population: 72,500 (release), 26,248 (updates); 3,122 ddebs |
| PyPI | top 5,000 projects by downloads from hugovk's `top-pypi-packages.min.json` (last_update 2026-11-01, 15,000 rows), then `pypi.org/pypi/<name>/json`: every file of the latest release with its `size`; plus 90 ML projects not all in the top 5,000; plus the PEP 691/700 JSON simple index (`/simple/<name>/`, all releases) for 24 ML giants | 4,999 projects, 38,509 files (33,773 wheels, 4,719 sdists, 17 other) |
| Maven Central | (1) popular: every managed dependency of `spring-boot-dependencies:4.1.2` and `quarkus-bom:3.40.1` with their imported BOMs (49 BOMs resolved), `HEAD` on `repo1.maven.org` for the jar; (2) random: 3,000 random groupId:artifactId from the Central index (one random version each), `HEAD` on the main artifact (packaging from the POM when there is no jar); (3) full population of every classified file (sources, javadoc, all, shaded, exec, natives, …) in Central's incremental index chunks 939–941 (`.index/nexus-maven-repository-index.9xx.gz`, 3.58 M records, last-modified mostly 2026-09-13…2026-10-22), whose `i` field carries the size; (4) `HEAD` on ~45 known giants | popular: 3,896 GAV files probed, 3,763 jars found; random: 3,000 GAs, 2,735 main artifacts with a body (jar 1,856, klib 590, aar 256, esa 25, war 4, nar 3), 254 pom-only; 1,137,690 classified files |
| npm | top 5,000 of `npm-high-impact@1.13.0`'s `topDownload` list (15,916 packages with ≥ 1 M weekly downloads, sorted by downloads); `registry.npmjs.org/<name>/latest` for `dist.tarball` and `dist.unpackedSize`, then a 1-byte Range GET on the tarball and the total from `Content-Range` (the registry's HEAD carries no `Content-Length`); plus 60 hand-picked binary-heavy packages | 4,996 + 29 tarballs |
| raw | GitHub REST `releases/latest` of 50 hand-picked projects known for large assets (Electron, ComfyUI, Ollama, llama.cpp, FreeCAD, Talos, …), unauthenticated | 1,373 assets, 942 without checksums/signatures/SBOMs — **biased to large on purpose**, read it as "what big generic artifacts look like", not as a population |

Metadata positions: Range reads of the first 4 KiB of the largest .debs (`ar` headers). For
wheels: Range reads of the last 128 KiB plus the central directory, giving the local-header
offset of `*.dist-info/METADATA` (13 large and 80 random wheels). For sdists and npm tarballs:
streamed gunzip over Range reads, stopped at `PKG-INFO` / `package.json`. The 6 largest sdists
and 7 large npm tarballs were read until the file was found; 50 random sdists were read whole when
≤ 2 MiB and for their first 1 MiB otherwise; 40 random npm tarballs were read whole. Network:
User-Agent `lajstrom-size-survey (contact: https://github.com/lajstrom/lajstrom)`, ≤ 8
concurrent requests per host, global back-off on 429/5xx (none happened). Percentiles are
nearest-rank. Histograms have log2 buckets [2^k, 2^(k+1)) bytes, and the bars are linear in
count. "." marks a non-empty bucket too small for one `#`.

## 2. Debian and Ubuntu (.deb)

| population | count | p50 | p90 | p99 | p99.9 | max | > 100 MiB | > 1 GiB | > 4 GiB |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| Debian 13 trixie, amd64+all, main+contrib+non-free+non-free-firmware | 69,913 | 57.5 KiB | 1.6 MiB | 23.7 MiB | 215 MiB | 3.0 GiB | 162 (0.23%) | 3 (0.004%) | 0 |
| Ubuntu 24.04 noble (release pocket), amd64+all, main+universe+restricted+multiverse | 72,500 | 54.3 KiB | 2.3 MiB | 35.6 MiB | 186 MiB | 2.7 GiB | 180 (0.25%) | 4 (0.006%) | 0 |
| Ubuntu noble-updates, amd64+all, 4 components | 26,248 | 823 KiB | 37.3 MiB | 108 MiB | 307 MiB | 1.2 GiB | 419 (1.60%) | 1 (0.004%) | 0 |
| Debian trixie-debug main (-dbgsym) | 21,085 | 253 KiB | 7.8 MiB | 113 MiB | 492 MiB | 1.2 GiB | 242 (1.15%) | 2 (0.009%) | 0 |
| Ubuntu ddebs noble + noble-updates main (-dbgsym) | 3,122 | 181 KiB | 32.8 MiB | 1.9 GiB | 2.0 GiB | 2.0 GiB | 275 (8.81%) | 236 (7.56%) | 0 |
| **Debian trixie + Ubuntu noble release, together** | 142,413 | 55.9 KiB | 1.9 MiB | 28.9 MiB | 201 MiB | 3.0 GiB | 342 (0.24%) | 7 (0.005%) | 0 |

### Debian 13 trixie (main + contrib + non-free + non-free-firmware), amd64

```
           bucket     count   share
      512 B–1 KiB       245   0.35% #
      1 KiB–2 KiB       872   1.25% ####
      2 KiB–4 KiB       891   1.27% ####
      4 KiB–8 KiB     5,237   7.49% ##########################
     8 KiB–16 KiB     9,215  13.18% #############################################
    16 KiB–32 KiB    10,200  14.59% ##################################################
    32 KiB–64 KiB     9,631  13.78% ###############################################
   64 KiB–128 KiB     8,022  11.47% #######################################
  128 KiB–256 KiB     6,556   9.38% ################################
  256 KiB–512 KiB     5,357   7.66% ##########################
    512 KiB–1 MiB     4,136   5.92% ####################
      1 MiB–2 MiB     3,339   4.78% ################
      2 MiB–4 MiB     2,258   3.23% ###########
      4 MiB–8 MiB     1,375   1.97% #######
     8 MiB–16 MiB     1,451   2.08% #######
    16 MiB–32 MiB       591   0.85% ###
    32 MiB–64 MiB       292   0.42% #
   64 MiB–128 MiB       120   0.17% #
  128 MiB–256 MiB        67   0.10% .
  256 MiB–512 MiB        37   0.05% .
    512 MiB–1 GiB        18   0.03% .
      1 GiB–2 GiB         2   0.00% .
      2 GiB–4 GiB         1   0.00% .
```

| # | name | size | bytes |
|---:|---|---:|---:|
| 1 | `qgis-api-doc` 3.40.6+dfsg-1 (main, installs to 3.7 GiB) | 3.0 GiB | 3,179,722,260 |
| 2 | `0ad-data` 0.27.0-1 (main, installs to 3.3 GiB) | 1.3 GiB | 1,389,812,248 |
| 3 | `ceph-test-dbg` 18.2.7+ds-1+deb13u1 (main, installs to 1.3 GiB) | 1.2 GiB | 1,296,512,180 |
| 4 | `unidic-mecab` 3.1.1-1 (main, installs to 4.8 GiB) | 993 MiB | 1,041,525,140 |
| 5 | `linux-image-6.12.107+deb13-rt-amd64-dbg` 6.12.107-1 (main, installs to 6.5 GiB) | 992 MiB | 1,040,278,468 |
| 6 | `linux-image-6.12.94+deb13-rt-amd64-dbg` 6.12.94-1 (main, installs to 6.5 GiB) | 992 MiB | 1,040,248,800 |
| 7 | `linux-image-6.12.94+deb13-amd64-dbg` 6.12.94-1 (main, installs to 6.5 GiB) | 992 MiB | 1,039,686,640 |
| 8 | `linux-image-6.12.107+deb13-amd64-dbg` 6.12.107-1 (main, installs to 6.5 GiB) | 991 MiB | 1,039,383,172 |
| 9 | `sauerbraten` 0.0.20201227-1 (non-free, installs to 1014 MiB) | 936 MiB | 981,629,962 |
| 10 | `nvidia-cuda-dev` 12.4.127~12.4.1-2 (non-free, installs to 2.4 GiB) | 891 MiB | 933,853,452 |

### Ubuntu 24.04 noble (release pocket), amd64

```
           bucket     count   share
      512 B–1 KiB     1,061   1.46% #####
      1 KiB–2 KiB       994   1.37% #####
      2 KiB–4 KiB     1,792   2.47% #########
      4 KiB–8 KiB     6,246   8.62% ################################
     8 KiB–16 KiB     9,433  13.01% #################################################
    16 KiB–32 KiB     9,698  13.38% ##################################################
    32 KiB–64 KiB     8,946  12.34% ##############################################
   64 KiB–128 KiB     7,252  10.00% #####################################
  128 KiB–256 KiB     6,329   8.73% #################################
  256 KiB–512 KiB     5,010   6.91% ##########################
    512 KiB–1 MiB     4,291   5.92% ######################
      1 MiB–2 MiB     3,638   5.02% ###################
      2 MiB–4 MiB     2,326   3.21% ############
      4 MiB–8 MiB     1,757   2.42% #########
     8 MiB–16 MiB     2,154   2.97% ###########
    16 MiB–32 MiB       782   1.08% ####
    32 MiB–64 MiB       406   0.56% ##
   64 MiB–128 MiB       249   0.34% #
  128 MiB–256 MiB        91   0.13% .
  256 MiB–512 MiB        29   0.04% .
    512 MiB–1 GiB        12   0.02% .
      1 GiB–2 GiB         3   0.00% .
      2 GiB–4 GiB         1   0.00% .
```

| # | name | size | bytes |
|---:|---|---:|---:|
| 1 | `qgis-api-doc` 3.34.4+dfsg-4build2 (universe, installs to 3.4 GiB) | 2.7 GiB | 2,893,772,000 |
| 2 | `0ad-data` 0.0.26-1 (universe, installs to 3.1 GiB) | 1.3 GiB | 1,377,404,936 |
| 3 | `flightgear-data-base` 1:2020.3.18+dfsg-1 (universe, installs to 1.7 GiB) | 1.3 GiB | 1,350,500,386 |
| 4 | `gcc-snapshot` 1:20240117-1ubuntu1 (universe, installs to 4.6 GiB) | 1.1 GiB | 1,168,345,496 |
| 5 | `unidic-mecab` 3.1.1-1 (universe, installs to 4.8 GiB) | 993 MiB | 1,041,525,188 |
| 6 | `sauerbraten` 0.0.20201227-1 (multiverse, installs to 1014 MiB) | 936 MiB | 981,630,214 |
| 7 | `redeclipse-data` 1.6.0-1 (universe, installs to 937 MiB) | 822 MiB | 862,262,832 |
| 8 | `alien-arena-data` 7.71.3+ds-1 (multiverse, installs to 1.8 GiB) | 778 MiB | 815,435,592 |
| 9 | `dotnet-sdk-8.0-source-built-artifacts` 8.0.104-0ubuntu1 (universe, installs to 739 MiB) | 696 MiB | 730,137,636 |
| 10 | `librocprim-tests` 5.7.1-2 (universe, installs to 1.8 GiB) | 662 MiB | 694,308,362 |

### Ubuntu noble-updates, amd64

The updates pocket is skewed upward: it is mostly kernels, kernel modules (restricted: 10,504 of
26,248), toolchains and other big packages that keep being rebuilt.

| # | name | size | bytes |
|---:|---|---:|---:|
| 1 | `dotnet-sdk-10.0-source-built-artifacts` 10.0.114-0ubuntu1~24.04.1 (universe, installs to 1.2 GiB) | 1.2 GiB | 1,241,861,230 |
| 2 | `dotnet-sdk-8.0-source-built-artifacts` 8.0.133-0ubuntu1~24.04.1 (universe, installs to 719 MiB) | 676 MiB | 709,030,716 |
| 3 | `mysql-source-8.0` 8.0.46-0ubuntu0.24.04.4 (universe, installs to 478 MiB) | 474 MiB | 496,619,560 |
| 4 | `mysql-testsuite-8.0` 8.0.46-0ubuntu0.24.04.4 (universe, installs to 801 MiB) | 385 MiB | 403,349,278 |
| 5 | `openjdk-25-dbg` 25.0.5.0.3+1-1~24.04.1 (universe, installs to 372 MiB) | 360 MiB | 377,899,334 |
| 6 | `linux-source-6.8.0` 6.8.0-149.149 (main, installs to 345 MiB) | 342 MiB | 358,317,300 |
| 7 | `linux-source-6.8.0` 6.8.0-147.147 (main, installs to 344 MiB) | 341 MiB | 357,287,488 |
| 8 | `linux-source-6.8.0` 6.8.0-136.136 (main, installs to 344 MiB) | 341 MiB | 357,129,976 |
| 9 | `linux-source-6.8.0` 6.8.0-142.142 (main, installs to 344 MiB) | 341 MiB | 357,053,416 |
| 10 | `linux-source-6.8.0` 6.8.0-137.137 (main, installs to 344 MiB) | 340 MiB | 356,976,774 |

### Debug symbols: Debian trixie-debug and Ubuntu ddebs

Relevant because in-house build pipelines often publish `-dbgsym` next to the binaries.

Debian trixie-debug main:

```
           bucket     count   share
      512 B–1 KiB         1   0.00% .
      1 KiB–2 KiB         4   0.02% .
      2 KiB–4 KiB       164   0.78% ####
      4 KiB–8 KiB       438   2.08% #########
     8 KiB–16 KiB     1,087   5.16% #######################
    16 KiB–32 KiB     2,036   9.66% ############################################
    32 KiB–64 KiB     2,337  11.08% ##################################################
   64 KiB–128 KiB     2,335  11.07% ##################################################
  128 KiB–256 KiB     2,175  10.32% ###############################################
  256 KiB–512 KiB     2,042   9.68% ############################################
    512 KiB–1 MiB     1,880   8.92% ########################################
      1 MiB–2 MiB     1,805   8.56% #######################################
      2 MiB–4 MiB     1,517   7.19% ################################
      4 MiB–8 MiB     1,197   5.68% ##########################
     8 MiB–16 MiB       797   3.78% #################
    16 MiB–32 MiB       547   2.59% ############
    32 MiB–64 MiB       337   1.60% #######
   64 MiB–128 MiB       213   1.01% #####
  128 MiB–256 MiB       114   0.54% ##
  256 MiB–512 MiB        40   0.19% #
    512 MiB–1 GiB        17   0.08% .
      1 GiB–2 GiB         2   0.01% .
```

| # | name | size | bytes |
|---:|---|---:|---:|
| 1 | `libopenfoam-dbgsym` 1912.200626-3+b1 (main, installs to 1.3 GiB) | 1.2 GiB | 1,332,964,092 |
| 2 | `python3-graph-tool-dbgsym` 2.91+ds-5 (main, installs to 1.4 GiB) | 1.1 GiB | 1,226,956,472 |
| 3 | `libdeal.ii-9.6.2-dbgsym` 9.6.2-4 (main, installs to 1.5 GiB) | 1017 MiB | 1,066,297,928 |
| 4 | `polymake-dbgsym` 4.12-3+b2 (main, installs to 1.0 GiB) | 1001 MiB | 1,049,410,848 |
| 5 | `libnode115-dbgsym` 20.19.2+dfsg-1+deb13u2 (main, installs to 1011 MiB) | 987 MiB | 1,035,055,372 |
| 6 | `qtcreator-dbgsym` 16.0.1-2 (main, installs to 960 MiB) | 922 MiB | 966,801,608 |
| 7 | `gcc-mingw-w64-bootstrap-dbgsym` 14.2.0-19+27+b1 (main, installs to 917 MiB) | 798 MiB | 837,183,716 |
| 8 | `kicad-dbgsym` 9.0.2+dfsg-1 (main, installs to 810 MiB) | 782 MiB | 820,020,468 |
| 9 | `klayout-dbgsym` 0.30.0-1+b1 (main, installs to 781 MiB) | 709 MiB | 743,511,792 |
| 10 | `sumo-dbgsym` 1.18.0+dfsg-4+b2 (main, installs to 711 MiB) | 676 MiB | 708,699,316 |

Ubuntu ddebs (noble + noble-updates, main). This index is small and dominated by kernel images:

```
           bucket     count   share
      1 KiB–2 KiB         1   0.03% .
      2 KiB–4 KiB       110   3.52% ###############
      4 KiB–8 KiB       123   3.94% ################
     8 KiB–16 KiB       196   6.28% ##########################
    16 KiB–32 KiB       248   7.94% #################################
    32 KiB–64 KiB       337  10.79% #############################################
   64 KiB–128 KiB       373  11.95% ##################################################
  128 KiB–256 KiB       331  10.60% ############################################
  256 KiB–512 KiB       329  10.54% ############################################
    512 KiB–1 MiB       233   7.46% ###############################
      1 MiB–2 MiB       208   6.66% ############################
      2 MiB–4 MiB       126   4.04% #################
      4 MiB–8 MiB        98   3.14% #############
     8 MiB–16 MiB        57   1.83% ########
    16 MiB–32 MiB        38   1.22% #####
    32 MiB–64 MiB        26   0.83% ###
   64 MiB–128 MiB        22   0.70% ###
  128 MiB–256 MiB         8   0.26% #
  256 MiB–512 MiB         8   0.26% #
    512 MiB–1 GiB        14   0.45% ##
      1 GiB–2 GiB       236   7.56% ################################
```

| # | name | size | bytes |
|---:|---|---:|---:|
| 1 | `linux-image-unsigned-7.0.0-31-generic-dbgsym` 7.0.0-31.31~24.04.1 (main, installs to 2.3 GiB) | 2.0 GiB | 2,112,572,920 |
| 2 | `linux-image-unsigned-7.0.0-28-generic-dbgsym` 7.0.0-28.28~24.04.1 (main, installs to 2.3 GiB) | 2.0 GiB | 2,111,546,292 |
| 3 | `linux-image-unsigned-7.0.0-30-generic-dbgsym` 7.0.0-30.30~24.04.1 (main, installs to 2.3 GiB) | 2.0 GiB | 2,111,483,420 |
| 4 | `linux-image-unsigned-7.0.0-34-generic-dbgsym` 7.0.0-34.34~24.04.1 (main, installs to 2.3 GiB) | 2.0 GiB | 2,111,482,916 |
| 5 | `linux-image-unsigned-7.0.0-29-generic-dbgsym` 7.0.0-29.29~24.04.2 (main, installs to 2.3 GiB) | 2.0 GiB | 2,111,432,212 |
| 6 | `linux-image-unsigned-7.0.0-14-generic-dbgsym` 7.0.0-14.14~24.04.3 (main, installs to 2.3 GiB) | 2.0 GiB | 2,109,636,144 |
| 7 | `linux-image-unsigned-7.0.0-1016-oracle-dbgsym` 7.0.0-1016.18~24.04.1 (main, installs to 2.3 GiB) | 2.0 GiB | 2,108,665,960 |
| 8 | `linux-image-unsigned-7.0.0-1011-oracle-dbgsym` 7.0.0-1011.11~24.04.1 (main, installs to 2.3 GiB) | 2.0 GiB | 2,105,131,488 |
| 9 | `linux-image-unsigned-7.0.0-1013-oracle-dbgsym` 7.0.0-1013.13~24.04.1 (main, installs to 2.3 GiB) | 2.0 GiB | 2,104,177,524 |
| 10 | `linux-image-unsigned-7.0.0-1012-oracle-dbgsym` 7.0.0-1012.12~24.04.1 (main, installs to 2.3 GiB) | 2.0 GiB | 2,102,831,100 |

### Layout of the largest .debs (ar headers read with a 4 KiB Range GET)

| package | .deb size | ar members (name: offset, size) | control.tar ends at |
|---|---:|---|---:|
| qgis-api-doc 3.40.6+dfsg-1 | 3.0 GiB | `debian-binary` @68 (4 B); `control.tar.xz` @132 (910 KiB); `data.tar.xz` @931,620 (3.0 GiB) | 931,560 B |
| 0ad-data 0.27.0-1 | 1.3 GiB | `debian-binary` @68 (4 B); `control.tar.xz` @132 (1020 B); `data.tar.xz` @1,212 (1.3 GiB) | 1,152 B |
| ceph-test-dbg 18.2.7+ds-1+deb13u1 | 1.2 GiB | `debian-binary` @68 (4 B); `control.tar.xz` @132 (5.2 KiB); `data.tar.xz` @5,556 (1.2 GiB) | 5,496 B |
| qgis-api-doc 3.34.4+dfsg-4build2 | 2.7 GiB | `debian-binary` @68 (4 B); `control.tar.zst` @132 (909 KiB); `data.tar.zst` @931,148 (2.7 GiB) | 931,087 B |
| 0ad-data 0.0.26-1 | 1.3 GiB | `debian-binary` @68 (4 B); `control.tar.xz` @132 (1.0 KiB); `data.tar.xz` @1,236 (1.3 GiB) | 1,176 B |
| flightgear-data-base 1:2020.3.18+dfsg-1 | 1.3 GiB | `debian-binary` @68 (4 B); `control.tar.zst` @132 (269 KiB); `data.tar.zst` @275,698 (1.3 GiB) | 275,637 B |
| libopenfoam-dbgsym 1912.200626-3+b1 | 1.2 GiB | `debian-binary` @68 (4 B); `control.tar.xz` @132 (6.1 KiB); `data.tar.xz` @6,428 (1.2 GiB) | 6,368 B |
| python3-graph-tool-dbgsym 2.91+ds-5 | 1.1 GiB | `debian-binary` @68 (4 B); `control.tar.xz` @132 (1.3 KiB); `data.tar.xz` @1,480 (1.1 GiB) | 1,420 B |
| libdeal.ii-9.6.2-dbgsym 9.6.2-4 | 1017 MiB | `debian-binary` @68 (4 B); `control.tar.xz` @132 (608 B); `data.tar.xz` @800 (1017 MiB) | 740 B |
| linux-image-unsigned-6.8.0-1005-ibm-dbgsym 6.8.0-1005.5 | 1.6 GiB | `debian-binary` @68 (4 B); `control.tar.xz` @132 (161 KiB); `data.tar.xz` @164,604 (1.6 GiB) | 164,544 B |
| linux-image-unsigned-6.8.0-1005-oracle-dbgsym 6.8.0-1005.5 | 1.6 GiB | `debian-binary` @68 (4 B); `control.tar.xz` @132 (162 KiB); `data.tar.xz` @166,452 (1.6 GiB) | 166,392 B |
| linux-image-unsigned-6.8.0-31-lowlatency-dbgsym 6.8.0-31.31.1 | 1.6 GiB | `debian-binary` @68 (4 B); `control.tar.xz` @132 (164 KiB); `data.tar.xz` @168,160 (1.6 GiB) | 168,100 B |
| dotnet-sdk-10.0-source-built-artifacts 10.0.114-0ubuntu1~24.04.1 | 1.2 GiB | `debian-binary` @68 (4 B); `control.tar.zst` @132 (671 B); `data.tar.zst` @864 (1.2 GiB) | 803 B |
| dotnet-sdk-8.0-source-built-artifacts 8.0.133-0ubuntu1~24.04.1 | 676 MiB | `debian-binary` @68 (4 B); `control.tar.zst` @132 (667 B); `data.tar.zst` @860 (676 MiB) | 799 B |
| mysql-source-8.0 8.0.46-0ubuntu0.24.04.4 | 474 MiB | `debian-binary` @68 (4 B); `control.tar.zst` @132 (731 B); `data.tar.zst` @924 (474 MiB) | 863 B |

`debian-binary` always sits at byte 68 and `control.tar.*` at byte 132. The control member is
0.6–6 KiB for most of these packages, 160–270 KiB for the kernel dbgsym and
`flightgear-data-base`, and 910 KiB at most (`qgis-api-doc`, whose md5sums list tens of
thousands of files). Everything Lajstrom needs from a .deb
has arrived before byte 931,620 of a 3.0 GiB file. Lajstrom's `MaxControl` is 4 MiB.

## 3. PyPI

| population | count | p50 | p90 | p99 | p99.9 | max | > 100 MiB | > 1 GiB | > 4 GiB |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| top-5000, all files of the latest release | 38,509 | 530 KiB | 7.4 MiB | 84.0 MiB | 382 MiB | 690 MiB | 269 (0.70%) | 0 | 0 |
|   wheels | 33,773 | 672 KiB | 8.6 MiB | 87.7 MiB | 388 MiB | 690 MiB | 265 (0.78%) | 0 | 0 |
|   sdists | 4,719 | 80.6 KiB | 1.9 MiB | 32.0 MiB | 99.5 MiB | 435 MiB | 4 (0.085%) | 0 | 0 |
| top-5000, largest file per project | 4,999 | 111 KiB | 5.5 MiB | 102 MiB | 534 MiB | 690 MiB | 51 (1.02%) | 0 | 0 |
| top-5000 + ML extras, all files | 38,616 | 532 KiB | 7.7 MiB | 88.1 MiB | 461 MiB | 924 MiB | 308 (0.80%) | 0 | 0 |

### Wheels (top-5000, latest release)

```
           bucket     count   share
      512 B–1 KiB         1   0.00% .
      1 KiB–2 KiB        15   0.04% .
      2 KiB–4 KiB        78   0.23% #
      4 KiB–8 KiB       335   0.99% ####
     8 KiB–16 KiB       917   2.72% ###########
    16 KiB–32 KiB     1,450   4.29% #################
    32 KiB–64 KiB     2,162   6.40% #########################
   64 KiB–128 KiB     2,727   8.07% ###############################
  128 KiB–256 KiB     3,215   9.52% #####################################
  256 KiB–512 KiB     4,330  12.82% ##################################################
    512 KiB–1 MiB     4,020  11.90% ##############################################
      1 MiB–2 MiB     4,244  12.57% #################################################
      2 MiB–4 MiB     3,616  10.71% ##########################################
      4 MiB–8 MiB     3,110   9.21% ####################################
     8 MiB–16 MiB     1,567   4.64% ##################
    16 MiB–32 MiB       738   2.19% #########
    32 MiB–64 MiB       741   2.19% #########
   64 MiB–128 MiB       285   0.84% ###
  128 MiB–256 MiB       141   0.42% ##
  256 MiB–512 MiB        63   0.19% #
    512 MiB–1 GiB        18   0.05% .
```

### Sdists (top-5000, latest release)

```
           bucket     count   share
      512 B–1 KiB         5   0.11% .
      1 KiB–2 KiB        10   0.21% #
      2 KiB–4 KiB        86   1.82% ######
      4 KiB–8 KiB       240   5.09% ##################
     8 KiB–16 KiB       515  10.91% #######################################
    16 KiB–32 KiB       667  14.13% ##################################################
    32 KiB–64 KiB       645  13.67% ################################################
   64 KiB–128 KiB       602  12.76% #############################################
  128 KiB–256 KiB       543  11.51% #########################################
  256 KiB–512 KiB       432   9.15% ################################
    512 KiB–1 MiB       292   6.19% ######################
      1 MiB–2 MiB       228   4.83% #################
      2 MiB–4 MiB       164   3.48% ############
      4 MiB–8 MiB       137   2.90% ##########
     8 MiB–16 MiB        65   1.38% #####
    16 MiB–32 MiB        40   0.85% ###
    32 MiB–64 MiB        30   0.64% ##
   64 MiB–128 MiB        14   0.30% #
  128 MiB–256 MiB         3   0.06% .
  256 MiB–512 MiB         1   0.02% .
```

### Largest files (top-5000 plus the ML extras, latest release)

| # | name | size | bytes |
|---:|---|---:|---:|
| 1 | `libcugraph_cu12-26.10.0-py3-none-manylinux_2_27_x86_64.manylinux_2_28_x86_64.whl` (rank extra) | 924 MiB | 969,141,177 |
| 2 | `libcugraph_cu12-26.10.0-py3-none-manylinux_2_26_aarch64.manylinux_2_28_aarch64.whl` (rank extra) | 920 MiB | 964,588,601 |
| 3 | `paddlepaddle_gpu-2.6.2-cp311-cp311-manylinux1_x86_64.whl` (rank extra) | 724 MiB | 758,955,369 |
| 4 | `paddlepaddle_gpu-2.6.2-cp310-cp310-manylinux1_x86_64.whl` (rank extra) | 724 MiB | 758,943,654 |
| 5 | `paddlepaddle_gpu-2.6.2-cp312-cp312-manylinux1_x86_64.whl` (rank extra) | 724 MiB | 758,943,234 |
| 6 | `paddlepaddle_gpu-2.6.2-cp39-cp39-manylinux1_x86_64.whl` (rank extra) | 724 MiB | 758,942,556 |
| 7 | `paddlepaddle_gpu-2.6.2-cp38-cp38-manylinux1_x86_64.whl` (rank extra) | 724 MiB | 758,940,867 |
| 8 | `nvidia_cudnn_cu12-9.28.1.4-py3-none-manylinux_2_27_aarch64.whl` (rank 844) | 690 MiB | 723,285,364 |
| 9 | `nvidia_cudnn_cu12-9.28.1.4-py3-none-manylinux_2_27_x86_64.whl` (rank 844) | 683 MiB | 716,579,350 |
| 10 | `nvidia_cudnn_cu12-9.28.1.4-py3-none-win_amd64.whl` (rank 844) | 664 MiB | 696,300,805 |
| 11 | `libcudf_cu12-26.10.2-py3-none-manylinux_2_27_x86_64.manylinux_2_28_x86_64.whl` (rank extra) | 636 MiB | 666,845,575 |
| 12 | `libcudf_cu12-26.10.2-py3-none-manylinux_2_26_aarch64.manylinux_2_28_aarch64.whl` (rank extra) | 632 MiB | 662,312,910 |
| 13 | `sgl_kernel-0.3.21-cp310-abi3-manylinux2014_aarch64.whl` (rank extra) | 598 MiB | 626,572,173 |
| 14 | `nvidia_cudnn_cu13-9.28.1.4-py3-none-manylinux_2_27_aarch64.whl` (rank 683) | 569 MiB | 597,008,823 |
| 15 | `nvidia_cublas_cu12-12.9.3.13-py3-none-manylinux_2_27_x86_64.whl` (rank 852) | 554 MiB | 581,217,458 |

### Largest sdists

| # | name | size | bytes |
|---:|---|---:|---:|
| 1 | `pyspark-4.3.0.tar.gz` | 435 MiB | 455,910,862 |
| 2 | `robosuite-1.5.2.tar.gz` | 148 MiB | 155,629,116 |
| 3 | `opencv_contrib_python_headless-5.0.0.93.tar.gz` | 147 MiB | 154,148,527 |
| 4 | `opencv_contrib_python-5.0.0.93.tar.gz` | 147 MiB | 154,110,105 |
| 5 | `comfyui_workflow_templates_media_video-0.3.101.tar.gz` | 99.5 MiB | 104,340,610 |
| 6 | `comfyui_workflow_templates_media_api-0.3.84.tar.gz` | 95.5 MiB | 100,128,925 |
| 7 | `comfyui_workflow_templates_media_assets_01-0.1.48.tar.gz` | 89.8 MiB | 94,157,647 |
| 8 | `flashinfer_python-0.7.3.tar.gz` | 86.9 MiB | 91,128,922 |
| 9 | `threejs_materials-1.2.5.tar.gz` | 85.7 MiB | 89,863,794 |
| 10 | `comfyui_workflow_templates_media_image-0.3.160.tar.gz` | 85.2 MiB | 89,352,862 |

### The ML giants (latest release)

| project | latest | rank | files | largest manylinux x86_64 wheel | largest file | which |
|---|---|---:|---:|---:|---:|---|
| libcugraph-cu12 | 26.10.0 | - | 2 | 924 MiB | 924 MiB | `libcugraph_cu12-26.10.0-py3-none-manylinux_2_27_x86_64.manylinux_2_28_x86_64.whl` |
| paddlepaddle-gpu | 2.6.2 | - | 10 | 724 MiB | 724 MiB | `paddlepaddle_gpu-2.6.2-cp311-cp311-manylinux1_x86_64.whl` |
| nvidia-cudnn-cu12 | 9.28.1.4 | 844 | 3 | 683 MiB | 690 MiB | `nvidia_cudnn_cu12-9.28.1.4-py3-none-manylinux_2_27_aarch64.whl` |
| libcudf-cu12 | 26.10.2 | - | 2 | 636 MiB | 636 MiB | `libcudf_cu12-26.10.2-py3-none-manylinux_2_27_x86_64.manylinux_2_28_x86_64.whl` |
| sgl-kernel | 0.3.21 | - | 2 | 511 MiB | 598 MiB | `sgl_kernel-0.3.21-cp310-abi3-manylinux2014_aarch64.whl` |
| nvidia-cudnn-cu13 | 9.28.1.4 | 683 | 4 | 493 MiB | 569 MiB | `nvidia_cudnn_cu13-9.28.1.4-py3-none-manylinux_2_27_aarch64.whl` |
| nvidia-cublas-cu12 | 12.9.3.13 | 852 | 2 | 554 MiB | 554 MiB | `nvidia_cublas_cu12-12.9.3.13-py3-none-manylinux_2_27_x86_64.whl` |
| tf-nightly | 2.23.0.dev20261109 | - | 12 | 549 MiB | 549 MiB | `tf_nightly-2.23.0.dev20261109-cp314-cp314-manylinux_2_27_x86_64.whl` |
| tensorflow | 2.21.0 | 1040 | 16 | 546 MiB | 546 MiB | `tensorflow-2.21.0-cp313-cp313-manylinux_2_27_x86_64.whl` |
| nvidia-cublas | 13.10.0.20 | 693 | 4 | 424 MiB | 534 MiB | `nvidia_cublas-13.10.0.20-py3-none-manylinux2014_aarch64.manylinux_2_17_aarch64.whl` |
| torch | 2.15.0 | 397 | 28 | 514 MiB | 514 MiB | `torch-2.15.0-cp312-cp312-manylinux_2_28_x86_64.whl` |
| nvidia-cublas-cu11 | 11.11.3.6 | 3539 | 4 | 399 MiB | 407 MiB | `nvidia_cublas_cu11-11.11.3.6-py3-none-win_amd64.whl` |
| nvidia-dali-cuda120 | 2.4.0 | - | 2 | 405 MiB | 405 MiB | `nvidia_dali_cuda120-2.4.0-py3-none-manylinux_2_28_x86_64.whl` |
| nvidia-cudnn-cu11 | 9.10.2.21 | 3599 | 2 | 379 MiB | 379 MiB | `nvidia_cudnn_cu11-9.10.2.21-py3-none-manylinux_2_27_x86_64.whl` |
| nvidia-cusparse-cu12 | 12.5.10.65 | 897 | 3 | 349 MiB | 349 MiB | `nvidia_cusparse_cu12-12.5.10.65-py3-none-manylinux2014_x86_64.manylinux_2_17_x86_64.whl` |
| nvidia-nccl-cu12 | 2.32.3 | 646 | 2 | 335 MiB | 335 MiB | `nvidia_nccl_cu12-2.32.3-py3-none-manylinux_2_27_x86_64.whl` |
| tensorflow-cpu | 2.21.0 | 3496 | 8 | 262 MiB | 335 MiB | `tensorflow_cpu-2.21.0-cp313-cp313-win_amd64.whl` |
| nvidia-cusolver-cu12 | 11.7.5.82 | 899 | 3 | 322 MiB | 322 MiB | `nvidia_cusolver_cu12-11.7.5.82-py3-none-manylinux_2_27_x86_64.whl` |
| vllm | 0.33.0 | 2774 | 3 | 321 MiB | 321 MiB | `vllm-0.33.0-cp38-abi3-manylinux_2_28_x86_64.whl` |
| nvidia-nccl-cu13 | 2.32.3 | 654 | 2 | 291 MiB | 291 MiB | `nvidia_nccl_cu13-2.32.3-py3-none-manylinux_2_27_aarch64.whl` |
| nvidia-cusolver | 12.4.1.2 | 721 | 4 | 248 MiB | 291 MiB | `nvidia_cusolver-12.4.1.2-py3-none-manylinux_2_27_aarch64.whl` |
| triton | 3.9.0 | 473 | 8 | 273 MiB | 273 MiB | `triton-3.9.0-cp314-cp314t-manylinux_2_27_x86_64.manylinux_2_28_x86_64.whl` |
| nvidia-nvshmem-cu12 | 3.8.0 | 1827 | 2 | 268 MiB | 268 MiB | `nvidia_nvshmem_cu12-3.8.0-py3-none-manylinux2014_x86_64.manylinux_2_17_x86_64.whl` |
| onnxruntime-gpu | 1.31.1 | 2634 | 16 | 244 MiB | 244 MiB | `onnxruntime_gpu-1.31.1-cp313-cp313t-manylinux_2_28_x86_64.whl` |
| nvidia-cusparselt-cu12 | 0.8.1 | 1032 | 3 | 228 MiB | 228 MiB | `nvidia_cusparselt_cu12-0.8.1-py3-none-manylinux2014_x86_64.whl` |
| nvidia-cusparse | 12.9.4.2 | 722 | 4 | 220 MiB | 220 MiB | `nvidia_cusparse-12.9.4.2-py3-none-manylinux2014_x86_64.manylinux_2_17_x86_64.whl` |
| nvidia-cusparselt-cu13 | 0.10.0 | 720 | 3 | 162 MiB | 210 MiB | `nvidia_cusparselt_cu13-0.10.0-py3-none-manylinux2014_aarch64.whl` |
| nvidia-cusparse-cu11 | 11.7.5.86 | 4882 | 4 | 195 MiB | 195 MiB | `nvidia_cusparse_cu11-11.7.5.86-py3-none-manylinux2014_x86_64.whl` |
| nvidia-cufft-cu12 | 11.4.1.4 | 895 | 3 | 192 MiB | 192 MiB | `nvidia_cufft_cu12-11.4.1.4-py3-none-manylinux2014_x86_64.manylinux_2_17_x86_64.whl` |
| paddlepaddle | 3.3.1 | 4020 | 15 | 186 MiB | 186 MiB | `paddlepaddle-3.3.1-cp312-cp312-manylinux1_x86_64.whl` |
| jax-cuda12-pjrt | 0.12.0.1 | - | 2 | 183 MiB | 183 MiB | `jax_cuda12_pjrt-0.12.0.1-py3-none-manylinux_2_27_x86_64.whl` |
| nvidia-nvshmem-cu13 | 3.8.0 | 712 | 2 | 174 MiB | 174 MiB | `nvidia_nvshmem_cu13-3.8.0-py3-none-manylinux2014_x86_64.manylinux_2_17_x86_64.whl` |
| nvidia-cufft-cu11 | 10.9.0.58 | 4888 | 4 | 161 MiB | 164 MiB | `nvidia_cufft_cu11-10.9.0.58-py3-none-win_amd64.whl` |
| nvidia-cufft | 12.5.0.14 | 717 | 4 | 157 MiB | 158 MiB | `nvidia_cufft-12.5.0.14-py3-none-win_arm64.whl` |
| cupy-cuda12x | 14.2.0 | 3559 | 18 | 130 MiB | 141 MiB | `cupy_cuda12x-14.2.0-cp311-cp311-manylinux2014_aarch64.whl` |
| libtpu | 0.0.52 | 4425 | 7 | 131 MiB | 131 MiB | `libtpu-0.0.52-cp314-cp314t-manylinux_2_31_x86_64.whl` |
| jax-cuda13-pjrt | 0.12.0.1 | - | 2 | 129 MiB | 129 MiB | `jax_cuda13_pjrt-0.12.0.1-py3-none-manylinux_2_27_x86_64.whl` |
| nvidia-cusolver-cu11 | 11.4.1.48 | 4911 | 4 | 122 MiB | 122 MiB | `nvidia_cusolver_cu11-11.4.1.48-py3-none-manylinux2014_x86_64.whl` |
| cupy-cuda11x | 13.6.0 | - | 15 | 95.2 MiB | 108 MiB | `cupy_cuda11x-13.6.0-cp311-cp311-manylinux2014_aarch64.whl` |
| libcuml-cu12 | 26.10.0 | - | 2 | 94.5 MiB | 94.5 MiB | `libcuml_cu12-26.10.0-py3-none-manylinux_2_27_x86_64.manylinux_2_28_x86_64.whl` |

### The ML giants across all releases (PEP 700 sizes from the JSON simple index)

| project | files (all releases) | largest file ever | which | uploaded |
|---|---:|---:|---|---|
| torch | 987 | 873 MiB | `torch-2.10.0-cp314-cp314-manylinux_2_28_x86_64.whl` | 2026-01-21 |
| tensorflow | 1,761 | 615 MiB | `tensorflow-2.19.1-cp312-cp312-manylinux_2_17_x86_64.manylinux2014_x86_64.whl` | 2025-08-13 |
| tensorflow-gpu | 662 | 561 MiB | `tensorflow_gpu-2.11.0rc1-cp39-cp39-manylinux_2_17_x86_64.manylinux2014_x86_64.whl` | 2022-10-19 |
| nvidia-cudnn-cu12 | 175 | 817 MiB | `nvidia_cudnn_cu12-9.24.1.1-py3-none-manylinux_2_27_aarch64.whl` | 2026-09-01 |
| nvidia-cudnn-cu11 | 51 | 700 MiB | `nvidia_cudnn_cu11-8.9.4.25-py3-none-win_amd64.whl` | 2023-08-09 |
| paddlepaddle-gpu | 441 | 779 MiB | `paddlepaddle_gpu-2.0.0rc1-cp38-cp38-win_amd64.whl` | 2020-12-14 |
| libcugraph-cu12 | 15 | 1000 MiB | `libcugraph_cu12-26.8.0-py3-none-manylinux_2_26_aarch64.manylinux_2_28_aarch64.whl` | 2026-08-06 |
| jaxlib | 889 | 100 MiB | `jaxlib-0.5.1-cp313-cp313t-manylinux2014_x86_64.whl` | 2025-02-24 |
| jax-cuda12-plugin | 384 | 16.1 MiB | `jax_cuda12_plugin-0.5.3-cp313-cp313t-manylinux2014_x86_64.whl` | 2025-03-19 |
| jax-cuda12-pjrt | 77 | 183 MiB | `jax_cuda12_pjrt-0.12.0-py3-none-manylinux_2_27_x86_64.whl` | 2026-10-19 |
| mxnet-cu112 | 4 | 568 MiB | `mxnet_cu112-2.0.0b1-py3-none-manylinux2014_x86_64.whl` | 2022-03-25 |
| mxnet-cu117 | 2 | 486 MiB | `mxnet_cu117-1.9.1-py3-none-manylinux2014_x86_64.whl` | 2022-08-29 |
| cupy-cuda12x | 276 | 141 MiB | `cupy_cuda12x-14.2.0-cp311-cp311-manylinux2014_aarch64.whl` | 2026-08-20 |
| nvidia-cublas-cu12 | 64 | 581 MiB | `nvidia_cublas_cu12-12.8.3.14-py3-none-manylinux_2_27_x86_64.whl` | 2025-01-23 |
| sgl-kernel | 86 | 598 MiB | `sgl_kernel-0.3.21-cp310-abi3-manylinux2014_aarch64.whl` | 2026-01-15 |
| flash-attn | 77 | 8.1 MiB | `flash_attn-2.8.3.post2.tar.gz` | 2026-10-14 |
| tensorrt-cu12-libs | 32 | 15.4 KiB | `tensorrt_cu12_libs-11.1.0.106.tar.gz` | 2026-06-16 |
| tensorrt-libs | 10 | 6.7 KiB | `tensorrt-libs-9.3.0.post12.dev1.tar.gz` | 2024-01-31 |
| tensorrt | 67 | 781 MiB | `tensorrt-8.6.0-cp39-none-manylinux_2_17_x86_64.whl` | 2023-03-14 |
| nvidia-tensorrt | 8 | 739 MiB | `nvidia_tensorrt-8.4.1.5-cp310-none-manylinux_2_17_x86_64.whl` | 2022-08-17 |
| xformers | 352 | 212 MiB | `xformers-0.0.26.post1-cp311-cp311-manylinux2014_x86_64.whl` | 2024-04-29 |
| vllm | 309 | 486 MiB | `vllm-0.15.1-cp38-abi3-manylinux_2_31_x86_64.whl` | 2026-02-05 |
| torch-xla | 43 | 102 MiB | `torch_xla-2.8.1-cp312-cp312-manylinux_2_28_x86_64.whl` | 2025-09-11 |
| pyspark | 77 | 439 MiB | `pyspark-4.2.0.dev3.tar.gz` | 2026-03-12 |

Nothing on pypi.org in these 24 projects (6,849 files) is over 1 GiB. That matches PyPI's
policy (docs.pypi.org/project-management/storage-limits): 100 MB per file by default (200 MB for
organisations), raised per project on request. The big files exist only by exception.
`tensorrt-cu12-libs` and `tensorrt-libs` are stub sdists of 7–15 KiB; their binaries are not on
pypi.org.

Off PyPI, PyTorch's own index serves the wheels that bundle CUDA. These are what internal
"pypi" repositories end up hosting or proxying:

| file on download-r2.pytorch.org | size |
|---|---:|
| `torch-2.9.1+cu128-cp312-cp312-manylinux_2_28_x86_64.whl` | 859 MiB (900,881,648 B) |
| `torch-2.1.2+cu121-cp311-cp311-linux_x86_64.whl` | 2.05 GiB (2,200,691,132 B) |
| `torch-2.0.1+cu118-cp311-cp311-linux_x86_64.whl` | 2.11 GiB (2,267,330,315 B) |

## 4. Maven Central

| population | count | p50 | p90 | p99 | p99.9 | max | > 100 MiB | > 1 GiB | > 4 GiB |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| popular (Spring Boot 4.1.2 + Quarkus 3.40.1 BOMs), main jars | 3,763 | 31.3 KiB | 689 KiB | 17.5 MiB | 200 MiB | 242 MiB | 9 (0.24%) | 0 | 0 |
| random GAs uploaded 2026-09/10, main artifact | 2,735 | 39.6 KiB | 839 KiB | 27.8 MiB | 222 MiB | 288 MiB | 7 (0.26%) | 0 | 0 |
|   of which .jar | 1,856 | 38.5 KiB | 914 KiB | 41.3 MiB | 222 MiB | 237 MiB | 6 (0.32%) | 0 | 0 |
| index window, every -sources.jar | 491,447 | 16.8 KiB | 278 KiB | 3.8 MiB | 37.6 MiB | 580 MiB | 186 (0.038%) | 0 | 0 |
| index window, every -javadoc.jar | 471,200 | 111 KiB | 3.0 MiB | 8.1 MiB | 42.7 MiB | 699 MiB | 164 (0.035%) | 0 | 0 |
| index window, every other classified file (all/shaded/exec/tests/natives/...) | 175,043 | 3.6 KiB | 1.0 MiB | 52.5 MiB | 316 MiB | 941 MiB | 804 (0.46%) | 0 | 0 |

### Popular (Spring Boot + Quarkus BOMs): main jars

```
           bucket     count   share
      256 B–512 B         7   0.19% .
      512 B–1 KiB         7   0.19% .
      1 KiB–2 KiB         6   0.16% .
      2 KiB–4 KiB        72   1.91% #####
      4 KiB–8 KiB       499  13.26% ####################################
     8 KiB–16 KiB       608  16.16% ###########################################
    16 KiB–32 KiB       700  18.60% ##################################################
    32 KiB–64 KiB       528  14.03% ######################################
   64 KiB–128 KiB       410  10.90% #############################
  128 KiB–256 KiB       286   7.60% ####################
  256 KiB–512 KiB       185   4.92% #############
    512 KiB–1 MiB       172   4.57% ############
      1 MiB–2 MiB       115   3.06% ########
      2 MiB–4 MiB        59   1.57% ####
      4 MiB–8 MiB        37   0.98% ###
     8 MiB–16 MiB        32   0.85% ##
    16 MiB–32 MiB        15   0.40% #
    32 MiB–64 MiB         7   0.19% .
   64 MiB–128 MiB        11   0.29% #
  128 MiB–256 MiB         7   0.19% .
```

| # | name | size | bytes |
|---:|---|---:|---:|
| 1 | `org.apache.pulsar:pulsar-io-hbase:4.2.5` | 242 MiB | 254,247,104 |
| 2 | `org.apache.pulsar:pulsar-io-jdbc-openmldb:4.2.5` | 210 MiB | 220,170,882 |
| 3 | `org.apache.pulsar:pulsar-io-solr:4.2.5` | 204 MiB | 213,924,229 |
| 4 | `org.apache.pulsar:pulsar-io-redis:4.2.5` | 200 MiB | 210,012,849 |
| 5 | `org.apache.pulsar:pulsar-io-influxdb:4.2.5` | 200 MiB | 209,964,646 |
| 6 | `org.apache.pulsar:pulsar-io-rabbitmq:4.2.5` | 197 MiB | 207,037,987 |
| 7 | `org.apache.pulsar:pulsar-io-kinesis:4.2.5` | 139 MiB | 145,922,511 |
| 8 | `org.apache.pulsar:pulsar-io-debezium-oracle:4.2.5` | 118 MiB | 124,017,992 |
| 9 | `org.apache.pulsar:pulsar-functions-local-runner:4.2.5` | 111 MiB | 115,910,731 |
| 10 | `org.apache.pulsar:pulsar-io-alluxio:4.2.5` | 97.4 MiB | 102,098,993 |

The popular set's tail is Apache Pulsar's IO connectors. They are self-contained jars of
100–240 MiB that `spring-boot-dependencies` pulls in by importing `org.apache.pulsar:pulsar-bom`.

### Random 2026 uploads: main artifact (jar, klib, aar, war, …)

```
           bucket     count   share
      256 B–512 B        14   0.51% ##
      512 B–1 KiB        20   0.73% ##
      1 KiB–2 KiB        14   0.51% ##
      2 KiB–4 KiB       137   5.01% #################
      4 KiB–8 KiB       250   9.14% ###############################
     8 KiB–16 KiB       401  14.66% #################################################
    16 KiB–32 KiB       407  14.88% ##################################################
    32 KiB–64 KiB       390  14.26% ################################################
   64 KiB–128 KiB       348  12.72% ###########################################
  128 KiB–256 KiB       221   8.08% ###########################
  256 KiB–512 KiB       164   6.00% ####################
    512 KiB–1 MiB       127   4.64% ################
      1 MiB–2 MiB        89   3.25% ###########
      2 MiB–4 MiB        49   1.79% ######
      4 MiB–8 MiB        33   1.21% ####
     8 MiB–16 MiB        24   0.88% ###
    16 MiB–32 MiB        20   0.73% ##
    32 MiB–64 MiB        14   0.51% ##
   64 MiB–128 MiB         7   0.26% #
  128 MiB–256 MiB         5   0.18% #
  256 MiB–512 MiB         1   0.04% .
```

| # | name | size | bytes |
|---:|---|---:|---:|
| 1 | `org.frankframework:frankframework-test:10.3.0` (war) | 288 MiB | 301,824,955 |
| 2 | `org.hl7.fhir.publisher:org.hl7.fhir.publisher.cli:3.0.1` (jar) | 237 MiB | 248,015,785 |
| 3 | `io.kestra.plugin:plugin-ai:2.2.0` (jar) | 222 MiB | 232,789,934 |
| 4 | `com.farao-community.farao:gridcapa-core-valid-intraday-app:0.7.0-beta` (jar) | 217 MiB | 227,343,521 |
| 5 | `org.exist-db:exist-installer:7.0.0-beta5` (jar) | 157 MiB | 164,368,961 |
| 6 | `org.apache.polaris.tools.iceberg-catalog-migrator:iceberg-catalog-migrator-cli:1.1.0` (jar) | 137 MiB | 144,087,275 |
| 7 | `org.finos.fluxnova.bpm.run:fluxnova-bpm-run-core:3.1.0` (jar) | 121 MiB | 126,357,389 |
| 8 | `org.apache.flink:flink-sql-connector-tidb-cdc:3.7.0-2.2` (jar) | 71.6 MiB | 75,106,804 |
| 9 | `io.dekorate:issue-1271-containers-empty-in-deployment:5.0.0` (jar) | 71.4 MiB | 74,878,354 |
| 10 | `io.dekorate:spring-boot-on-openshift-example:5.0.0` (jar) | 71.4 MiB | 74,878,310 |

### Every classified file uploaded in the index window (fat/shaded/exec jars, natives, sources, javadoc)

```
           bucket     count   share
          2 B–4 B         1   0.00% .
          4 B–8 B         0   0.00% 
         8 B–16 B         3   0.00% .
        16 B–32 B       159   0.09% .
        32 B–64 B        12   0.01% .
       64 B–128 B       296   0.17% .
      128 B–256 B       326   0.19% .
      256 B–512 B    41,797  23.88% ##################################################
      512 B–1 KiB    14,871   8.50% ##################
      1 KiB–2 KiB    11,940   6.82% ##############
      2 KiB–4 KiB    19,550  11.17% #######################
      4 KiB–8 KiB    12,917   7.38% ###############
     8 KiB–16 KiB    12,289   7.02% ###############
    16 KiB–32 KiB     8,621   4.93% ##########
    32 KiB–64 KiB     8,456   4.83% ##########
   64 KiB–128 KiB     8,047   4.60% ##########
  128 KiB–256 KiB     6,809   3.89% ########
  256 KiB–512 KiB     5,918   3.38% #######
    512 KiB–1 MiB     5,386   3.08% ######
      1 MiB–2 MiB     4,885   2.79% ######
      2 MiB–4 MiB     4,026   2.30% #####
      4 MiB–8 MiB     2,880   1.65% ###
     8 MiB–16 MiB     1,623   0.93% ##
    16 MiB–32 MiB     1,510   0.86% ##
    32 MiB–64 MiB     1,490   0.85% ##
   64 MiB–128 MiB       658   0.38% #
  128 MiB–256 MiB       355   0.20% .
  256 MiB–512 MiB       136   0.08% .
    512 MiB–1 GiB        82   0.05% .
```

Largest per groupId:artifactId:classifier:

| # | name | size | bytes |
|---:|---|---:|---:|
| 1 | `edu.stanford.nlp:stanford-corenlp:4.6.0`, classifier `models-chinese`, .jar | 941 MiB | 986,617,202 |
| 2 | `org.apache.knox:gateway:2.1.1`, classifier `source-release`, .zip | 920 MiB | 964,216,525 |
| 3 | `org.eclipse.dirigible:dirigible-application:14.58.0`, classifier `executable`, .jar | 846 MiB | 887,001,783 |
| 4 | `com.codbex.atlas:codbex-atlas-application:14.76.0`, classifier `executable`, .jar | 839 MiB | 879,797,226 |
| 5 | `com.nvidia:cudf-spark-jni:26.10.0`, classifier `cuda12`, .jar | 798 MiB | 836,479,065 |
| 6 | `com.nvidia:cudf-spark-jni:26.10.0`, classifier `cuda12-arm64`, .jar | 780 MiB | 818,286,081 |
| 7 | `io.cdktn:cdktn-provider-awscc:1.8.0`, classifier `javadoc`, .jar | 699 MiB | 732,668,168 |
| 8 | `com.nvidia.cuopt:cuopt:26.10.0`, classifier `cuda12`, .jar | 674 MiB | 706,222,102 |
| 9 | `com.nvidia.cuopt:cuopt:26.10.0`, classifier `cuda12-arm64`, .jar | 670 MiB | 702,623,929 |
| 10 | `ai.rapids:cudf:26.10.2`, classifier `cuda12`, .jar | 660 MiB | 692,556,758 |

Of the 1,154 classified files over 100 MiB, the classifiers are: `sources` 186, `javadoc` 164, `all` 150, `exec` 116, `executable` 82, `shaded` 55, `production` 40, `jar-with-dependencies` 37, `bin` 20, `with-dependencies` 19, `boot` 16, `tests` 15. So
beyond sources and javadoc, the big ones are fat/shaded/executable jars (`all`, `exec`,
`executable`, `shaded`, `jar-with-dependencies`, `boot`) and native bundles (`cuda12`, …).

Note on the index: in the 2026 chunks, the record of a GAV's *main* artifact often carries the
size of a `.pom.sha512` file (128 B), not the size of the jar. That is why main-artifact sizes
were taken with `HEAD` and not from the index. The classified records are consistent with `HEAD`.

### Known giants (HEAD on repo1.maven.org, latest version unless noted)

| artifact | version | file | size |
|---|---|---|---:|
| org.bytedeco:cuda | 12.3-8.9-1.5.10 | cuda-12.3-8.9-1.5.10-linux-x86_64-redist.jar | 1.86 GiB (1,992,966,570 B) |
| org.bytedeco:cuda | 12.6-9.5-1.5.11 | cuda-12.6-9.5-1.5.11-linux-x86_64-redist.jar | 1.89 GiB (2,026,748,503 B) |
| ai.djl.pytorch:pytorch-native-cu124 | 2.5.1 (Last-Modified 2024-11-13) | pytorch-native-cu124-2.5.1-linux-x86_64.jar | 2.29 GiB (2,457,619,283 B) |
| software.amazon.awssdk:bundle | 2.57.12 | bundle-2.57.12.jar | 694 MiB |
| com.amazonaws:aws-java-sdk-bundle | 1.12.797 | aws-java-sdk-bundle-1.12.797.jar | 371 MiB |
| org.bytedeco:pytorch | 2.13.0-1.5.14 | pytorch-2.13.0-1.5.14-linux-x86_64-gpu.jar | 434 MiB |
| org.bytedeco:tensorrt | 11.2-1.5.14 | tensorrt-11.2-1.5.14-linux-x86_64-redist.jar | 531 MiB |
| org.bytedeco:tensorflow-lite | 2.21.0-1.5.14 | tensorflow-lite-2.21.0-1.5.14-linux-x86_64.jar | 4.1 MiB |
| ai.djl.pytorch:pytorch-native-cu124 | 2.5.1 | pytorch-native-cu124-2.5.1-linux-x86_64.jar | 2.3 GiB |
| ai.djl.pytorch:pytorch-native-cpu | 2.7.1 | pytorch-native-cpu-2.7.1-linux-x86_64.jar | 106 MiB |
| com.microsoft.onnxruntime:onnxruntime_gpu | 1.31.1 | onnxruntime_gpu-1.31.1.jar | 385 MiB |
| com.microsoft.onnxruntime:onnxruntime | 1.31.1 | onnxruntime-1.31.1.jar | 48.6 MiB |
| org.apache.spark:spark-core_2.13 | 4.3.0 | spark-core_2.13-4.3.0.jar | 15.6 MiB |
| org.jetbrains.kotlin:kotlin-compiler-embeddable | 2.5.0-Beta2 | kotlin-compiler-embeddable-2.5.0-Beta2.jar | 56.4 MiB |
| org.jetbrains.kotlin:kotlin-compiler | 2.5.0-Beta2 | kotlin-compiler-2.5.0-Beta2.jar | 57.9 MiB |
| edu.stanford.nlp:stanford-corenlp | 4.6.0 | stanford-corenlp-4.6.0-models-english.jar | 424 MiB |
| edu.stanford.nlp:stanford-corenlp | 4.6.0 | stanford-corenlp-4.6.0-models-chinese.jar | 941 MiB |
| org.apache.hadoop:hadoop-client-runtime | 3.5.0 | hadoop-client-runtime-3.5.0.jar | 28.7 MiB |
| org.apache.hadoop:hadoop-client-api | 3.5.0 | hadoop-client-api-3.5.0.jar | 18.9 MiB |
| com.facebook.presto:presto-server | 0.300 | presto-server-0.300.tar.gz | 655 MiB |
| io.prestosql:presto-server | 350 | presto-server-350.tar.gz | 473 MiB |
| org.sonarsource.sonarqube:sonar-application | 26.11.0.134175 | sonar-application-26.11.0.134175.zip | 873 MiB |
| org.elasticsearch:elasticsearch | 9.5.6 | elasticsearch-9.5.6.jar | 21.0 MiB |
| org.nd4j:nd4j-cuda-11.6 | 1.0.0-M2.1 | nd4j-cuda-11.6-1.0.0-M2.1-linux-x86_64-cudnn.jar | 573 MiB |
| org.nd4j:nd4j-native | 1.0.0-M2.1 | nd4j-native-1.0.0-M2.1-linux-x86_64-avx2.jar | 79.3 MiB |
| org.openjfx:javafx-web | 28-ea+12 | javafx-web-28-ea+12-linux.jar | 44.0 MiB |
| ai.djl.tensorflow:tensorflow-native-cu113 | 2.10.1 | tensorflow-native-cu113-2.10.1-linux-x86_64.jar | 464 MiB |
| org.tensorflow:libtensorflow_jni_gpu | 1.15.0 | libtensorflow_jni_gpu-1.15.0.jar | 355 MiB |
| org.tensorflow:tensorflow-core-native | 1.2.0 | tensorflow-core-native-1.2.0-linux-x86_64-gpu.jar | 407 MiB |
| io.zonky.test.postgres:embedded-postgres-binaries-linux-amd64 | 18.6.0 | embedded-postgres-binaries-linux-amd64-18.6.0.jar | 14.4 MiB |
| org.graalvm.python:python-resources | 25.5.5.0.3 | python-resources-25.5.5.0.3.jar | 26.6 MiB |
| org.graalvm.js:js-language | 25.5.5.0.3 | js-language-25.5.5.0.3.jar | 26.8 MiB |

The only jar over 2 GiB found is DJL's `pytorch-native-cu124` 2.5.1 (2.29 GiB, Last-Modified
2024-11-13). bytedeco's CUDA redist jar is 1.86 GiB in 1.5.10 (2024-01) and 1.89 GiB in 1.5.11
(2024-11). 1.5.12 (2025-06-30) is on Central without its `-redist` jars, and so is the latest
release, 1.5.14. Nothing over 941 MiB appears in the 2026 window. This suggests that today's
publishing path caps files at about 1 GB, but it was not verified against Sonatype's
documentation.

## 5. npm

| population | count | p50 | p90 | p99 | p99.9 | max | > 100 MiB | > 1 GiB | > 4 GiB |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| top-5000 by downloads, tarball (.tgz) | 4,996 | 8.1 KiB | 201 KiB | 6.3 MiB | 41.6 MiB | 152 MiB | 2 (0.040%) | 0 | 0 |
| top-5000, dist.unpackedSize | 4,674 | 35.1 KiB | 1.0 MiB | 20.4 MiB | 152 MiB | 326 MiB | 6 (0.13%) | 0 | 0 |
| top-5000 + 60 hand-picked binary/large packages, tarball | 5,025 | 8.2 KiB | 213 KiB | 8.7 MiB | 43.6 MiB | 152 MiB | 3 (0.060%) | 0 | 0 |

```
           bucket     count   share
      256 B–512 B         3   0.06% .
      512 B–1 KiB        15   0.30% #
      1 KiB–2 KiB       346   6.93% ###############
      2 KiB–4 KiB       989  19.80% ############################################
      4 KiB–8 KiB     1,126  22.54% ##################################################
     8 KiB–16 KiB       765  15.31% ##################################
    16 KiB–32 KiB       514  10.29% #######################
    32 KiB–64 KiB       352   7.05% ################
   64 KiB–128 KiB       249   4.98% ###########
  128 KiB–256 KiB       196   3.92% #########
  256 KiB–512 KiB       132   2.64% ######
    512 KiB–1 MiB       130   2.60% ######
      1 MiB–2 MiB        51   1.02% ##
      2 MiB–4 MiB        42   0.84% ##
      4 MiB–8 MiB        44   0.88% ##
     8 MiB–16 MiB        26   0.52% #
    16 MiB–32 MiB         6   0.12% .
    32 MiB–64 MiB         8   0.16% .
   64 MiB–128 MiB         0   0.00% 
  128 MiB–256 MiB         2   0.04% .
```

| # | name | size | bytes |
|---:|---|---:|---:|
| 1 | `@anthropic-ai/claude-agent-sdk-linux-x64@0.3.322` (rank 4641, unpacked 326 MiB) | 152 MiB | 159,262,449 |
| 2 | `@anthropic-ai/claude-code-linux-x64@2.1.322` (rank 3085, unpacked 326 MiB) | 152 MiB | 159,262,440 |
| 3 | `onnxruntime-node@1.31.1` (hand-picked, unpacked 293 MiB) | 110 MiB | 115,391,311 |
| 4 | `@oven/bun-linux-x64@1.4.4` (hand-picked, unpacked 102 MiB) | 50.0 MiB | 52,418,437 |
| 5 | `@pnpm/linux-x64@11.28.11` (rank 3965, unpacked 144 MiB) | 46.4 MiB | 48,687,548 |
| 6 | `@cloudflare/workerd-linux-64@1.20261110.1` (rank 1828, unpacked 152 MiB) | 43.6 MiB | 45,716,782 |
| 7 | `next@16.4.6` (rank 3204, unpacked 183 MiB) | 41.6 MiB | 43,665,499 |
| 8 | `aws-cdk-lib@2.275.0` (hand-picked, unpacked 133 MiB) | 36.1 MiB | 37,858,953 |
| 9 | `@next/swc-linux-x64-gnu@16.4.6` (rank 1565, unpacked 96.2 MiB) | 33.6 MiB | 35,233,522 |
| 10 | `expo-modules-core@58.0.30` (rank 3778, unpacked 35.4 MiB) | 33.3 MiB | 34,940,191 |
| 11 | `onnxruntime-web@1.31.1` (hand-picked, unpacked 144 MiB) | 33.0 MiB | 34,586,352 |
| 12 | `@biomejs/cli-linux-x64-musl@2.6.6` (rank 4650, unpacked 92.5 MiB) | 32.9 MiB | 34,546,389 |
| 13 | `@next/swc-linux-x64-musl@16.4.6` (rank 3598, unpacked 93.9 MiB) | 32.9 MiB | 34,537,589 |
| 14 | `@biomejs/cli-linux-x64@2.6.6` (rank 3182, unpacked 92.3 MiB) | 32.7 MiB | 34,298,406 |
| 15 | `@next/swc-darwin-arm64@16.4.6` (hand-picked, unpacked 86.3 MiB) | 31.0 MiB | 32,471,888 |

## 6. Raw / generic: GitHub release assets (biased sample, see section 1)

| population | count | p50 | p90 | p99 | p99.9 | max | > 100 MiB | > 1 GiB | > 4 GiB |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| latest release of 50 projects, assets without checksums/signatures/SBOMs | 942 | 37.0 MiB | 347 MiB | 1.4 GiB | 2.0 GiB | 2.0 GiB | 284 (30.15%) | 22 (2.34%) | 0 |

```
           bucket     count   share
       64 B–128 B        18   1.91% ####
      128 B–256 B         6   0.64% #
      256 B–512 B         1   0.11% .
      512 B–1 KiB         1   0.11% .
      1 KiB–2 KiB         1   0.11% .
      2 KiB–4 KiB         2   0.21% .
      4 KiB–8 KiB         4   0.42% #
     8 KiB–16 KiB         7   0.74% ##
    16 KiB–32 KiB         1   0.11% .
    32 KiB–64 KiB         5   0.53% #
   64 KiB–128 KiB         5   0.53% #
  128 KiB–256 KiB        10   1.06% ##
  256 KiB–512 KiB         6   0.64% #
    512 KiB–1 MiB        12   1.27% ###
      1 MiB–2 MiB        25   2.65% ######
      2 MiB–4 MiB        27   2.87% #######
      4 MiB–8 MiB         9   0.96% ##
     8 MiB–16 MiB        89   9.45% ######################
    16 MiB–32 MiB       204  21.66% ##################################################
    32 MiB–64 MiB       157  16.67% ######################################
   64 MiB–128 MiB       135  14.33% #################################
  128 MiB–256 MiB        94   9.98% #######################
  256 MiB–512 MiB        58   6.16% ##############
    512 MiB–1 GiB        43   4.56% ###########
      1 GiB–2 GiB        22   2.34% #####
```

| # | name | size | bytes |
|---:|---|---:|---:|
| 1 | electron/electron `electron-v45.2.3-win32-x64-pdb.zip` (v45.2.3) | 2.0 GiB | 2,098,344,706 |
| 2 | comfyanonymous/ComfyUI `ComfyUI_windows_portable_nvidia.7z` (v0.44.0) | 1.9 GiB | 2,054,382,457 |
| 3 | comfyanonymous/ComfyUI `ComfyUI_windows_portable_nvidia_cu126.7z` (v0.44.0) | 1.8 GiB | 1,945,636,677 |
| 4 | comfyanonymous/ComfyUI `ComfyUI_windows_portable_nvidia_arm64.7z` (v0.44.0) | 1.8 GiB | 1,890,391,373 |
| 5 | ollama/ollama `OllamaSetup.exe` (v0.40.16) | 1.7 GiB | 1,857,663,944 |
| 6 | comfyanonymous/ComfyUI `ComfyUI_windows_portable_amd.7z` (v0.44.0) | 1.6 GiB | 1,671,700,941 |
| 7 | ollama/ollama `ollama-windows-amd64.zip` (v0.40.16) | 1.5 GiB | 1,618,244,995 |
| 8 | ollama/ollama `ollama-linux-arm64.tar.zst` (v0.40.16) | 1.5 GiB | 1,600,610,073 |
| 9 | ollama/ollama `ollama-linux-amd64.tar.zst` (v0.40.16) | 1.4 GiB | 1,545,767,640 |
| 10 | electron/electron `electron-v45.2.3-darwin-arm64-dsym.tar.xz` (v45.2.3) | 1.4 GiB | 1,534,617,236 |

GitHub's documented limit is "each file included in a release must be under 2 GiB"
(docs.github.com, About releases). The largest assets (Electron PDBs, ComfyUI portable bundles,
Ollama bundles with CUDA) sit right under it.

## 7. Where the metadata sits, and whether streaming with parse is realistic

What Lajstrom parses today (read from the code, `internal/format/*`, unchanged):

| format | what is parsed | how it reads the upload today |
|---|---|---|
| raw | nothing; digest only | `format.Spool`, then digest and push |
| deb | `debian-binary`, `control.tar.*` (≤ `MaxControl` = 4 MiB), md5/sha1/sha256 of the whole file | spooled; `deb.Parse` is already one pass and does not open `data.tar` |
| pypi wheel | `*.dist-info/METADATA` via `archive/zip` | spooled; `ReadWheelMetadata(io.ReaderAt, size)` needs random access (central directory) |
| pypi sdist | `<root>/PKG-INFO` | spooled; `readTarGzFile` is a stream reader bounded to 1 GiB unpacked / 2^18 entries |
| maven | POM only (≤ `maxPOM` = 4 MiB); jars and everything else only get md5/sha1/sha256/sha512 + size | spooled (`r.up.Open()`), read once to digest |
| npm | `package.json` plus the whole tar walk (bin paths, install scripts, entry/size bounds) | JSON body streamed; `_attachments` base64 decoded on the fly into a spool, then read whole |

### Wheels: where `*.dist-info/METADATA` sits (Range reads of EOCD + central directory)

| wheel | size | entries | METADATA local header at | entries after it | central dir | zip64 | data-descriptor entries |
|---|---:|---:|---:|---:|---:|---|---:|
| `libcugraph_cu12-26.10.0-py3-none-manylinux_2_27_x86_64.manylinux_2_28_x86_64.whl` | 924 MiB | 2,710 | 99.958% (924 MiB) | 7 | 277 KiB | no | 0 |
| `paddlepaddle_gpu-2.6.2-cp311-cp311-manylinux1_x86_64.whl` | 724 MiB | 2,489 | 99.953% (723 MiB) | 4 | 236 KiB | no | 0 |
| `nvidia_cudnn_cu12-9.28.1.4-py3-none-manylinux_2_27_aarch64.whl` | 690 MiB | 33 | 99.999% (690 MiB) | 3 | 2.7 KiB | no | 0 |
| `libcudf_cu12-26.10.2-py3-none-manylinux_2_27_x86_64.manylinux_2_28_x86_64.whl` | 636 MiB | 2,998 | 99.935% (636 MiB) | 5 | 294 KiB | no | 0 |
| `sgl_kernel-0.3.21-cp310-abi3-manylinux2014_aarch64.whl` | 598 MiB | 1,002 | 99.974% (597 MiB) | 3 | 102 KiB | no | 0 |
| `nvidia_cublas_cu12-12.9.3.13-py3-none-manylinux_2_27_x86_64.whl` | 554 MiB | 14 | 99.999% (554 MiB) | 3 | 1.2 KiB | no | 0 |
| `tf_nightly-2.23.0.dev20261109-cp314-cp314-manylinux_2_27_x86_64.whl` | 549 MiB | 26,002 | 0.175% (982 KiB) | 25997 | 2.9 MiB | no | 0 |
| `tensorflow-2.21.0-cp313-cp313-manylinux_2_27_x86_64.whl` | 546 MiB | 24,117 | 0.163% (911 KiB) | 24112 | 2.7 MiB | no | 0 |
| `torch-2.15.0-cp312-cp312-manylinux_2_28_x86_64.whl` | 514 MiB | 13,187 | 99.649% (512 MiB) | 148 | 1.2 MiB | no | 0 |
| `jaxlib-0.12.0.1-cp315-cp315t-manylinux_2_27_x86_64.whl` | 82.9 MiB | 140 | 99.979% (82.9 MiB) | 3 | 10.8 KiB | no | 0 |
| `jax_cuda12_pjrt-0.12.0.1-py3-none-manylinux_2_27_x86_64.whl` | 183 MiB | 8 | 99.999% (183 MiB) | 4 | 699 B | no | 0 |
| `numpy-2.5.4-cp314-cp314t-musllinux_1_2_x86_64.whl` | 17.7 MiB | 1,049 | 99.087% (17.5 MiB) | 50 | 83.2 KiB | no | 0 |
| `pyarrow-26.0.1-cp314-cp314-musllinux_1_2_x86_64.whl` | 55.9 MiB | 813 | 99.790% (55.8 MiB) | 7 | 65.7 KiB | no | 0 |

Random sample of 80 wheels from the top-5000 latest releases: `.dist-info` at the end (METADATA among the last 7 entries) in 70; at the start in 6; elsewhere in 4. Where it is at the end, METADATA's local header is at most 589 KiB before EOF (median 11.6 KiB), central directory included. Wheels with any data-descriptor entry (sizes not in the local header): 0 (with a STORED data-descriptor entry: 0). Largest central directory in the random sample: 338 KiB.

`.dist-info` first: `pglast-8.7-cp315-cp315t-macosx_10_15_x86_64.whl`, `pyodbc-5.5.0-cp311-cp311-musllinux_1_2_x86_64.whl`, `ahocorasick_rs-1.0.3-cp314-cp314-macosx_10_12_x86_64.whl`, `coreforecast-0.0.20-cp315-cp315-macosx_10_15_x86_64.whl`, `psutil-7.2.2-cp314-cp314t-macosx_11_0_arm64.whl`, `pyppmd-1.3.1-cp311-cp311-macosx_10_9_x86_64.whl`

Elsewhere: `pycurl-7.49.0-cp314-cp314-macosx_10_15_x86_64.whl` (#8 of 31), `curated_tokenizers-2.0.1-cp310-cp310-macosx_10_9_x86_64.whl` (#8 of 41), `pyroaring-1.2.0-cp315-cp315-musllinux_1_2_aarch64.whl` (#9 of 16), `rasterio-1.5.2-cp312-cp312-manylinux_2_28_x86_64.whl` (#257 of 264)

### Sdists (.tar.gz): where `<name>-<version>/PKG-INFO` sits (streamed gunzip over Range reads, stopped at PKG-INFO)

| sdist | size | PKG-INFO is tar entry # | compressed bytes read until found (chunk granularity) |
|---|---:|---:|---:|
| `pyspark-4.3.0.tar.gz` | 435 MiB | 5 | ≤ 4.0 MiB (0.9%) |
| `robosuite-1.5.2.tar.gz` | 148 MiB | 5 | ≤ 4.0 MiB (2.7%) |
| `opencv_contrib_python_headless-5.0.0.93.tar.gz` | 147 MiB | 5 | ≤ 4.0 MiB (2.7%) |
| `opencv_contrib_python-5.0.0.93.tar.gz` | 147 MiB | 5 | ≤ 4.0 MiB (2.7%) |
| `comfyui_workflow_templates_media_video-0.3.101.tar.gz` | 99.5 MiB | 2 | ≤ 4.0 MiB (4.0%) |
| `comfyui_workflow_templates_media_api-0.3.84.tar.gz` | 95.5 MiB | 2 | ≤ 4.0 MiB (4.2%) |

Random sample of 50 .tar.gz sdists from the top-5000 (≤ 2 MiB read whole, larger ones only their first 1 MiB): PKG-INFO among the first 12 entries in 31; the last entry in 10, plus 2 larger ones where it is not in the first 1 MiB (`pylibjpeg_openjpeg-2.6.0.tar.gz`, `pandas-3.1.1.tar.gz`); in between in 0; 7 have fewer than 13 entries, too few to tell.

The ones with PKG-INFO last: `pycrdt_store-0.1.5.tar.gz` (#18 of 18), `hist-2.12.1.tar.gz` (#155 of 155), `pytest_env-1.8.5.tar.gz` (#33 of 33), `langchain_deepseek-1.1.1.tar.gz` (#24 of 24), `isort-9.0.2.tar.gz` (#190 of 190), `asyncclick-8.4.2.1.tar.gz` (#95 of 95), `dagster_aws-0.29.29.tar.gz` (#150 of 150), `countryinfo-1.0.1.tar.gz` (#285 of 285), `prime_tunnel-0.1.17.tar.gz` (#17 of 17), `oauth2client-4.1.3.tar.gz` (#113 of 113)

### npm tarballs: where `package/package.json` sits

| tarball | size | package.json is tar entry # | of files | compressed bytes read until found |
|---|---:|---:|---:|---:|
| `typescript@7.0.2` | 357 KiB | 117 | 416 | ≤ 192 KiB (54%) |
| `@cloudflare/workerd-linux-64@1.20261110.1` | 43.6 MiB | 2 | 3 | ≤ 43.6 MiB (100%) |
| `@anthropic-ai/claude-code-linux-x64@2.1.322` | 152 MiB | 2 | 4 | ≤ 152 MiB (100%) |
| `next@16.4.6` | 41.6 MiB | 3818 | 8711 | ≤ 17.0 MiB (41%) |
| `@pnpm/linux-x64@11.28.11` | 46.4 MiB | 3 | 5 | ≤ 46.4 MiB (100%) |
| `aws-cdk-lib@2.275.0` | 36.1 MiB | 4016 | 7641 | ≤ 27.0 MiB (75%) |
| `onnxruntime-node@1.31.1` | 110 MiB | 23 | 43 | ≤ 110 MiB (100%) |

Random 40 of the top-5000: package.json within the first 3 entries in 19 of 40; elsewhere in 21 (e.g. `@babel/plugin-transform-private-property-in-object@8.0.5` #6/6, `jscodeshift@17.4.0` #68/97, `dequal@2.0.3` #6/11, `@graphql-codegen/gql-tag-operations@7.0.0` #5/7, `react-dom@19.3.1` #42/43, `@emnapi/runtime@1.11.3` #9/17).

### Per-format verdict

- **deb: yes, easily.** `control.tar.*` is always the 2nd ar member at byte 132 and ends at
  ≤ 0.9 MiB even in the 3.0 GiB package. The parser is already single-pass, and the sums
  (md5/sha1/sha256 for `Packages`) are stream hashes. One twist: the OCI repository is named after
  the package (`EscapeComponent(package)`), and that name is inside `control`. So buffer the first
  ≤ 132 B + `MaxControl` in memory, parse, open the blob upload in the right repository, then
  stream the rest. A .deb never needs a temp file.
- **pypi sdist: yes, with a late verdict.** It is a stream format and is already read as one.
  All 6 of the largest sdists, the 435 MiB `pyspark` included, have PKG-INFO at entry #2 or #5,
  within the first 4 MiB. So do 31 of the 50 random ones. 12 of the 50 have it as the last
  entry. This matches the comment in `pypi/parse.go`: setuptools writes it early, while flit,
  hatchling, meson-python and others write it last. For those, a stream parse finds it at EOF,
  after the whole body has gone to the registry.
- **pypi wheel: yes, but it needs a streaming zip reader.** Most wheels follow the wheel spec's
  advice ("Archivers are encouraged to place the .dist-info files physically at the end of the
  archive"). In 70 of 80 random wheels, METADATA is among the last 7 entries. In the largest
  wheels it is at 99.65–99.999 % of the file: cuGraph, paddle, cuDNN, cuDF, cuBLAS, jaxlib, and
  torch, where 148 entries follow it. tensorflow and tf-nightly put `.dist-info` first (0.16 %),
  and so do 6 of the 80 random wheels, mostly macOS builds. Two ways to do it without a spool:
  1. Walk the local headers as the bytes pass and keep any `*.dist-info/METADATA` (bounded, a few
     KiB). Keep a bounded tail buffer for the central directory: up to 2.9 MiB measured
     (tf-nightly, 26,002 entries), so cap it at e.g. 8 MiB, close to the 2^18-entry bound
     `maxEntries` already applies. At EOF, check that the central directory names the same entry
     at the same offset. This prevents the local-header/central-directory confusion that would
     let Lajstrom index another METADATA than pip reads. None of the 93 wheels inspected uses
     data descriptors, but the reader must still handle them; a STORED entry with a data
     descriptor cannot be delimited without the central directory and should be refused.
  2. Push the blob first, then read EOCD → central directory → METADATA back from the registry
     with three Range GETs. This assumes the registry serves Range on blob GETs, which OCI makes
     optional.

  In both cases the verdict comes after the upload.
- **maven: yes, trivially.** Jars are never opened; only four digests and the size are computed.
  The POM is a separate small PUT. The OCI repository comes from the path.
- **npm: yes, with a late verdict.** The tarball is already read whole as a stream, and the base64
  is decoded on the fly. `package.json`'s place in the tar follows path order. In single-binary
  platform packages it comes after the binary, at 100 % of the file (`claude-code-linux-x64`
  152 MiB, `@pnpm/linux-x64`, `workerd`, `onnxruntime-node`). In big multi-file packages it is
  somewhere in the middle (`next` 41 %, `aws-cdk-lib` 75 %). The package name, and so the OCI
  repository, is in the URL and the JSON body. The cross-check against the tarball's
  `package.json` completes at EOF.
- **raw: yes, trivially.**

What streaming costs, for every format:

1. **Late rejection leaves an orphan blob.** If the upload is rejected after the blob was pushed
   (bad wheel METADATA, sdist PKG-INFO, or npm package.json mismatch at EOF), the blob stays in
   the registry with no manifest until registry GC. Nothing references it, so the "registry is
   the truth" invariant holds.
2. **The digest is known only at the end.** So the push must be a chunked upload
   (POST → PATCH… → PUT `?digest=`). Registries differ in chunked-upload support and chunk-size
   rules; this was not measured here.
3. **No retry from local bytes.** With a spool, a failed registry push can be retried. With
   streaming, the client must re-upload. A slow client also holds a registry upload session open
   for the whole transfer: 5 GiB at 50 Mbit/s is ~14 minutes.

## 8. (a) Temp-file budget per replica, and (c) maxFileSize defaults

### (a) Spool needed for N concurrent uploads

| format (population) | p99 | max observed | 8 × p99 | 32 × p99 | 1 × max + 7 × p99 | 8 × max | 32 × max |
|---|---:|---:|---:|---:|---:|---:|---:|
| raw (GitHub assets, biased large) | 1.4 GiB | 2.0 GiB (GitHub cap) | 11.2 GiB | 44.8 GiB | 11.8 GiB | 16.0 GiB | 64.0 GiB |
| deb (Debian + Ubuntu archives) | 28.9 MiB | 3.0 GiB | 231 MiB | 925 MiB | 3.2 GiB | 24.0 GiB | 96.0 GiB |
| deb, in-house (lead's 5 GiB packages) | (as above) | 5 GiB | 231 MiB | 925 MiB | 5.2 GiB | 40.0 GiB | 160 GiB |
| deb -dbgsym (Debian trixie-debug) | 113 MiB | 1.2 GiB | 904 MiB | 3.5 GiB | 2.0 GiB | 9.6 GiB | 38.4 GiB |
| pypi wheel (pypi.org) | 87.7 MiB | 924 MiB latest, 1000 MiB all-time | 702 MiB | 2.7 GiB | 1.6 GiB | 7.8 GiB | 31.3 GiB |
| pypi wheel (incl. download.pytorch.org) | 87.7 MiB | 2.11 GiB | 702 MiB | 2.7 GiB | 2.7 GiB | 16.9 GiB | 67.6 GiB |
| pypi sdist | 32.0 MiB | 435 MiB | 256 MiB | 1.0 GiB | 659 MiB | 3.4 GiB | 13.6 GiB |
| maven main artifact (random 2026) | 27.8 MiB | 288 MiB | 222 MiB | 890 MiB | 483 MiB | 2.25 GiB | 9.0 GiB |
| maven classified (fat/shaded/natives) | 52.5 MiB | 941 MiB (2026), 2.29 GiB (all-time) | 420 MiB | 1.6 GiB | 1.3 GiB / 2.6 GiB | 7.4 GiB / 18.3 GiB | 29.4 GiB / 73.3 GiB |
| npm tarball | 6.3 MiB | 152 MiB | 50.4 MiB | 202 MiB | 196 MiB | 1.2 GiB | 4.8 GiB |

With today's code, each upload occupies its full size in `$TMPDIR` for the whole request. npm
spools the decoded tarball, not the 4/3-larger base64. If `$TMPDIR` is an `emptyDir` with
`medium: Memory`, the spool counts against the pod's memory limit. Sizing:

- **Typical load.** At p99 the spool is small: < 1 GiB for 8 concurrent uploads of anything but
  raw.
- **Tail.** The real budget is "one whale plus the rest": 3.2 GiB for public debs, 5+ GiB with
  the lead's debs, ~1.6 GiB for ML wheels (2.7 GiB with PyTorch's CUDA wheels), ~1.3 GiB for
  Maven fat jars.
- **Adversarial.** Uploads are client-chosen, so the only safe static size is
  N × maxFileSize. For 8 parallel uploads with an 8 GiB deb/raw limit that is 64 GiB of
  ephemeral disk per replica, for a case that almost never happens.

  The cheap alternative is admission control: reserve the declared `Content-Length` (or the
  repo's maxFileSize when the body is chunked) against a per-replica spool budget, and answer
  503 + `Retry-After` when it is full. Then the spool can be sized for "a few whales" rather than the worst case.

### (c) Suggested maxFileSize defaults

Today the code applies no limit unless `policy.maxFileSize` is set (`MaxFileSize` returns -1).

| format | p99.9 | largest seen anywhere | suggested default | why |
|---|---:|---:|---:|---|
| npm | 41.6 MiB | 152 MiB (`claude-code-linux-x64`, `claude-agent-sdk-linux-x64`) | **512 MiB** | 3× the largest public tarball; npm clients send base64 inside JSON, so the body is ~683 MiB at that limit |
| pypi | 388 MiB (wheels) | 1000 MiB on pypi.org; 2.11 GiB on download.pytorch.org | **4 GiB** (1 GiB if only pypi.org-style content is expected) | covers the CUDA-bundled torch wheels that internal indexes host; pypi.org itself defaults to 100 MB |
| maven | 200–316 MiB | 941 MiB (2026 uploads), 2.29 GiB (DJL, 2024) | **4 GiB** | covers every jar ever seen on Central with headroom; main jars are ≤ 290 MiB |
| deb | 201 MiB | 3.0 GiB public; 5 GiB in-house (lead) | **8 GiB** | covers the lead's 5 GiB packages; the ar header's 10-digit size field caps a member at 9,999,999,999 B (9.31 GiB) anyway |
| raw | 2.0 GiB (GitHub sample) | 2.0 GiB (GitHub's cap) | **8 GiB** | generic bucket for images, models and bundles; the same as deb, so whatever a .deb may carry can also be published raw |

If spooling stays, the spool has to be sized for N × these defaults, unless admission control is
added. For 8 concurrent 8 GiB uploads that is 64 GiB. That is the strongest argument for streaming: with
streaming, maxFileSize is only a policy and costs no disk. Whatever the default, the ingress in
front of Lajstrom must allow it: `client_max_body_size`, proxy buffering off, long body timeouts.

## 9. Caveats

- PyPI and npm are popularity samples (top 5,000) of the latest release only. Older releases and
  the long tail of rarely downloaded packages are not covered, except through the all-release
  check of the ML giants and the 60+90 hand-picked extras. The long tail is probably smaller,
  not larger (not measured).
- The Maven random sample is uniform over groupId:artifactId uploaded in the index window
  (Kotlin multiplatform modules are a large share: 590 klib of 2,735). The popular sample is
  what two big BOMs manage.
- The GitHub sample is deliberately biased toward big-asset projects.
- The Debian/Ubuntu numbers are full populations of one architecture (amd64 plus arch:all).
  Other architectures have the same arch:all packages and similar binaries.
- ar/zip/tar positions were read from a handful of files (listed). Statements about "most"
  backends rest on the 50 random sdists and 80 random wheels.
