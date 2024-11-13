# Latest Docker Image
### usage:
```
Usage:  ldi  [OPTIONS] NAME[:TAG|:REGEX|@DIGEST]    

Options:
  -arch string    Architecture
  -os string      Operating Systetem 
```
### example:
```
ldi grafana/grafana-oss:^(\d+)\.(\d+)\.(\d+)$
ldi grafana/grafana-oss:latest
ldi portainer/portainer-ee:^(\d+)\.(\d+)\.(\d+)-alpine$

# pull docker latest images
docker pull $(ldi -tag '^(\d+)\.(\d+)\.(\d+)$' traefik)
docker pull $(ldi -tag -alpine$ golang)
docker pull $(ldi -tag '^(\d+)\.(\d+)\.(\d+)$' grafana/grafana-oss)

# pull docker latest image list from file 

image-list.txt content:
grafana/grafana-oss:^(\d+)\.(\d+)\.(\d+)$
grafana/grafana-oss:latest
portainer/portainer-ee:^(\d+)\.(\d+)\.(\d+)-alpine$

script:
while IFS= read -r line; do
    docker pull $(ldi $line)
done < image-list.txt

```
