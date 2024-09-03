CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o ./bin/ldi-linux ./cmd
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-w -s" -o ./bin/ldi-windows.exe ./cmd