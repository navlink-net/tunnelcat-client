.PHONY: build build-exit wintun test

# Windows GUI client
build: wintun
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
		go build -o shortnerdcat.exe -ldflags="-H windowsgui" ./win-client/cmd/shortnerdcat/

# Fetch wintun.dll (skipped if already present)
wintun:
	go run ./win-client/tools/fetch_wintun/

# Linux exit node
build-exit:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -o snc-exit/snc-exit ./snc-exit/

test:
	go test ./win-client/... -timeout=60s -count=1 -v
