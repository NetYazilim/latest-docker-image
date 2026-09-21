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
# pull.sh
while IFS= read -r line; do
    [[ "$line" =~ ^\s*# || -z "$line" ]] && continue
    docker pull $(ldi "$line")
done < image-list.txt
```

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
