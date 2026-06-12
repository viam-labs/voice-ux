MODULE_BINARY := bin/voice-ux

$(MODULE_BINARY): Makefile go.mod *.go cmd/module/*.go sounds/*.pcm
	GOOS=$(VIAM_BUILD_OS) GOARCH=$(VIAM_BUILD_ARCH) go build -o $(MODULE_BINARY) cmd/module/main.go

lint:
	gofmt -s -w .

update:
	go get go.viam.com/rdk@latest
	go mod tidy

test:
	go test ./...

sounds:
	go run ./etc/gen_sounds

module.tar.gz: meta.json $(MODULE_BINARY)
	strip $(MODULE_BINARY)
	tar czf $@ meta.json $(MODULE_BINARY)

module: test module.tar.gz

all: test module.tar.gz

setup:
	go mod tidy

clean:
	rm -rf bin module.tar.gz

.PHONY: lint update test sounds module all setup clean
