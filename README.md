# Latest Docker Image
### usage:
```
ldi  [OPTIONS] IMAGE[:TAG]    
Show information about the latest version of a Docker IMAGE in the Docker Hub.

Options:
  -arch string    Architecture, default: host architecture
  -os string      Operating System, default: linux

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

## Notes
- **Anchor version filters with `$`.** An unanchored `'(\d+)\.(\d+)\.(\d+)'`
  also matches variant tags such as `1.25.1-alpine`, and ldi may then return the
  variant instead of the plain version. Use `'(\d+)\.(\d+)\.(\d+)$'` to get
  `1.25.1`.
- Pre-release and floating tags are always skipped: `alpha`, `beta`, `rc`,
  `pre`, `preview`, `dev`, `snapshot`, `nightly`, `canary`, `edge` (as a
  `-`/`.`/`_` separated part of the tag) and `latest`. Tags ending in
  `-source` are skipped too: Red Hat registries publish a source container
  next to every image (`9.0.0-1468-source`), and it is not runnable.
- **Only Docker Hub is supported for now.** A reference that names a registry
  host (`registry.redhat.io/ubi9/ubi`, `quay.io/prometheus/node-exporter`) or
  pins an `@sha256:...` digest is parsed correctly but rejected with an
  explicit error, instead of being silently queried against Docker Hub.
- When no matching tag is found, ldi writes **nothing** to stdout and exits with
  code 1, so `docker pull $(ldi ...)` will not run with a bogus argument.
