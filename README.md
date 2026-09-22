# Latest Docker Image
### usage:
```
ldi  [OPTIONS] [REGISTRY/]IMAGE[:TAG]
Show information about the latest version of a container IMAGE in Docker Hub
or in any registry speaking OCI Distribution.

Options:
  -arch string    Architecture, default: host architecture
  -os string      Operating System, default: linux
  -verbose, -V    Report what the lookup cost: pages read, tags seen,
                  candidates left, manifest lookups, elapsed time

TAG filter options:
  empty                newest tag (pre-release/floating tags excluded)
  regular expression   tag filter - anchor it with $, see Notes
  @DIGEST              a specific digest (not supported yet; rejected)
```
### example:
```
 ldi grafana/grafana-oss
 ldi grafana/grafana-oss:'(\d+)\.(\d+)\.(\d+)$'
 ldi portainer/portainer-ee:'(\d+)\.(\d+)\.(\d+)-alpine$'

# other registries
 ldi registry.access.redhat.com/ubi9/ubi:'^9\.[0-9]+$'
 ldi quay.io/prometheus/node-exporter:'^v(\d+)\.(\d+)\.(\d+)$'

# use ldi with docker client
docker pull $(ldi traefik:'(\d+)\.(\d+)\.(\d+)$')
docker pull $(ldi golang:'-alpine$' )
docker pull $(ldi grafana/grafana-oss:'(\d+)\.(\d+)\.(\d+)$')
```
## pull docker latest image from list 
### image-list.txt content:
```
traefik # newest tag
# comment line  
grafana/grafana-oss:(\d+)\.(\d+)\.(\d+)$ 
portainer/portainer-ee:^(\d+)\.(\d+)\.(\d+)-alpine$
```
### script:
```
#!/usr/bin/env bash
# pull.sh
set -uo pipefail

while IFS= read -r line <&3 || [ -n "$line" ]; do
    line=${line%%#*}                            # drop an inline comment
    line=$(printf '%s' "$line" | tr -d '\r')    # tolerate a CRLF list
    line="${line#"${line%%[![:space:]]*}"}"     # trim leading blanks
    line="${line%"${line##*[![:space:]]}"}"     # trim trailing blanks
    [ -z "$line" ] && continue

    if ! image=$(ldi "$line" </dev/null); then
        echo "skipped: $line" >&2
        continue
    fi
    docker pull -q "$image"
done 3< image-list.txt
```
The list is read from file descriptor 3 so that `docker pull` cannot swallow the
rest of it from stdin, and an `ldi` failure is skipped rather than turned into a
`docker pull` with no argument.

## build
```
./build.sh                  # cross-compiles into ./bin/
VERSION=1.7.0 ./build.sh    # stamp an exact version
```
The version is injected at link time, so `ldi --version` reports what the binary
was built from: the nearest git tag, with a suffix when HEAD has moved past it
or the tree is dirty (`v1.7.0`, `v1.7.0-3-gabc1234-dirty`). A plain
`go build ./cmd` leaves it at `dev`.

## Notes
- **Anchor version filters with `$`.** An unanchored `'(\d+)\.(\d+)\.(\d+)'`
  also matches variant tags such as `1.25.1-alpine`. Between two tags of the
  same version the plain release wins, but a variant of a *higher* version does
  not: with `1.26.0-alpine` published and plain releases still at `1.25.1`, the
  unanchored filter returns the alpine image. Use
  `'(\d+)\.(\d+)\.(\d+)$'` for plain releases and
  `'(\d+)\.(\d+)\.(\d+)-alpine$'` for the variant.
- **Pin the major version to stay on a release line.**
  `'^2\.(\d+)\.(\d+)$'` tracks 2.x.x and never follows the repository to
  3.x. Escape the dot: `'^2.(\d+)'` would also match `2X5`.
- **A lookup on a Distribution registry costs what its tag list costs.** The
  list has to be read in full, because lexical order says nothing about which
  tag is newest, and the tag filter does not change that: `public.ecr.aws`
  reads 8825 tags in about 29 seconds whether 60 or 584 of them are candidates,
  while `gcr.io` answers 49223 tags in 4.5 seconds and Docker Hub finishes in
  under half a second. `-verbose` reports the numbers.

  `public.ecr.aws` has an API of its own behind the gallery web site, and it
  is not the way out: it answers anonymously and carries dates and digests,
  but it costs 5.6 ms per tag against the registry's 3.3 ms, pages at most a
  thousand names at a time and sorts by nothing, so the same repository takes
  about fifty seconds there rather than thirty. Measured, not assumed.
- Pre-release and floating tags are always skipped: `alpha`, `beta`, `rc`,
  `pre`, `preview`, `dev`, `snapshot`, `nightly`, `canary`, `edge` (as a
  `-`/`.`/`_` separated part of the tag) and `latest`. Tags ending in
  `-source` are skipped too: Red Hat registries publish a source container
  next to every image (`9.0.0-1468-source`), and it is not runnable. So are
  `.sig`, `.att` and `.sbom` tags, which cosign writes beside every image it
  signs - on `gcr.io/distroless/base` they are almost the entire tag list.
- **Registries.** A bare name, or a `docker.io/...` reference, goes to Docker
  Hub's own API, which answers with tag, platform and date in a single call.
  `registry.access.redhat.com` goes to Red Hat's container catalogue for the
  same reason. Every other host is spoken to over OCI Distribution: `quay.io`,
  `ghcr.io`, `mcr.microsoft.com`, `gcr.io`, `public.ecr.aws`, Harbor and so on.
  Only anonymous access is supported so far, so a registry that requires a login
  (`registry.redhat.io`, which the catalogue does not index either) says exactly
  that instead of returning a tag. An `@sha256:...` digest is parsed but
  rejected.

- **Red Hat goes through its catalogue, not its registry.**
  `registry.access.redhat.com` caps a page of tag names at 100 and carries no
  dates, so `ubi9/ubi` costs 34 sequential requests over Distribution. The
  catalogue behind `catalog.redhat.com` answers architecture, build date, digest
  and tag names in one sorted request, narrowed server-side to the architecture
  asked for, which also means no manifest lookup per candidate. Measured on the
  same host: `ubi9/ubi` 9.4s to 2.5s, `ubi8/ubi` 6.9s to 1.3s, same answers.
- The name written to stdout always carries the host, so
  `docker pull $(ldi quay.io/prometheus/node-exporter:'^v(\d+)\.(\d+)\.(\d+)$')`
  works. Where a registry cannot supply a date cheaply the tag line reports the
  manifest digest instead of an update time - a multi-architecture index
  carries no date at all.
- **A tag has to look like a version to win.** Dot-separated numbers, with an
  optional `v` and an optional suffix, qualify; a single number may be at most
  four digits, so `node:22` is a version while `20250101` and the epoch stamps
  Red Hat publishes beside `9.8` are build identifiers. When nothing matching
  the filter looks like a version - a repository tagged by commit hash, such as
  `gcr.io/distroless/base` - ldi says so rather than returning whichever tag the
  registry listed first.
- **Naming one tag overrides all of that.** A filter that is a plain literal
  (`latest`, `^latest$`, `^nonroot$`) is read as "I want this tag": the
  exclusions and the version rule step aside, so
  `ldi gcr.io/distroless/base:latest` reports that tag and its digest. Such a
  filter is matched **exactly**, not as a regular expression - `:latest` means
  the tag `latest`, never `latest-amd64`. Anything that selects among tags is
  still a regular expression and keeps the rules.
- **A broad filter on a huge repository is cut short.** Where the registry
  cannot order tags for us, ldi looks up at most 50 candidates before giving up
  and asking for a narrower filter, rather than issuing one request per tag.
  `gcr.io/distroless/base` answers `tags/list` with about 14 MB.
- When no matching tag is found, ldi writes **nothing** to stdout and exits with
  code 1, so `docker pull $(ldi ...)` will not run with a bogus argument.
