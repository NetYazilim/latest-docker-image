# Latest Docker Image
### usage:
```
ldi  [OPTIONS] IMAGE[:TAG]    
Show information about the latest version of a Docker IMAGE in the Docker Hub.

Options:
  -arch string    Architecture
  -os string      Operating Systetem, default: linux

TAG filter options:
  emty for latest tag
  regular expression for tag filter
  @DIGEST for a specific digest
```
### example:
```
 ldi grafana/grafana-oss
 ldi grafana/grafana-oss:'(\d+)\.(\d+)\.(\d+)'
 ldi portainer/portainer-ee:'(\d+)\.(\d+)\.(\d+)-alpine$'

# use ldi with docker client
docker pull $(ldi traefik:'(\d+)\.(\d+)\.(\d+)')
docker pull $(ldi golang:'-alpine$' )
docker pull $(ldi grafana/grafana-oss:'(\d+)\.(\d+)\.(\d+)')
```
## pull docker latest image from list 
### image-list.txt content:
```
traefik # latest tag
# comment line  
grafana/grafana-oss:(\d+)\.(\d+)\.(\d+) 
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
