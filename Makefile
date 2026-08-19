.PHONY: build test vet

# Windows GUI client. wintun.dll is not vendored in this repo — download it
# from https://www.wintun.net/ and place it next to the built binary before
# running (see README.md).
build:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
		go build -o shortnerdcat.exe -ldflags="-H windowsgui" ./snc/win/cmd/shortnerdcat/

test:
	go test ./... -timeout=60s -count=1 -v

vet:
	go vet ./...
