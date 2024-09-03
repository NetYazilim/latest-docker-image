# Latest Docker Image
```
# usage:
ldi -tag '^(\d+)\.(\d+)\.(\d+)$' traefik
ldi -tag '-alpine$' golang
ldi -tag '^(\d+)\.(\d+)\.(\d+)$' grafana/grafana-oss

# pull docker latest images
docker pull $(ldi -tag '^(\d+)\.(\d+)\.(\d+)$' traefik)
docker pull $(ldi -tag -alpine$ golang)
docker pull $(ldi -tag '^(\d+)\.(\d+)\.(\d+)$' grafana/grafana-oss)

```

