# Latest Docker Image
```
# usage:
ldi -repo traefik -tag '^(\d+)\.(\d+)\.(\d+)$'
ldi -repo golang -tag '-alpine$'
ldi -repo grafana/grafana-oss -tag '^(\d+)\.(\d+)\.(\d+)$'

# pull docker latest images
docker pull $(ldi -repo traefik -tag '^(\d+)\.(\d+)\.(\d+)$')
docker pull $(ldi -repo golang -tag -alpine$)
docker pull $(ldi -repo grafana/grafana-oss -tag '^(\d+)\.(\d+)\.(\d+)$')

```

