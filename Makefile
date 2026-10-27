VERSION ?= 0.1.0
GO ?= go
LDFLAGS := -s -w -X termio/internal/version.Version=$(VERSION)

.PHONY: build test vet fmt build-all debug-init clean

build:
	mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/termio ./cmd/termio
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/termio-gateway ./cmd/termio-gateway
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/termioctl ./cmd/termioctl
	@if [ "$$($(GO) env GOOS)" = "windows" ]; then \
		echo "building Windows GUI agent"; \
		$(GO) build -trimpath -ldflags "$(LDFLAGS) -H=windowsgui" -o bin/termio-agent.exe ./cmd/termio; \
	fi

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

build-all:
	mkdir -p dist
	@for t in windows/amd64 windows/arm64 darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do \
		os=$${t%/*}; arch=$${t#*/}; ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		echo "building termio $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "dist/termio-$$os-$$arch$$ext" ./cmd/termio || exit 1; \
		echo "building gateway $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "dist/termio-gateway-$$os-$$arch$$ext" ./cmd/termio-gateway || exit 1; \
		echo "building termioctl $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "dist/termioctl-$$os-$$arch$$ext" ./cmd/termioctl || exit 1; \
	done
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS) -H=windowsgui" -o dist/termio-agent-windows-amd64.exe ./cmd/termio
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS) -H=windowsgui" -o dist/termio-agent-windows-arm64.exe ./cmd/termio

debug-init:
	mkdir -p .debug
	$(GO) run ./cmd/termio init --config .debug/config.json --url ws://127.0.0.1:8787/ws --token dev-token --device-id dev_debug --device-name debug-device --workspace "$(CURDIR)" --log-file "$(CURDIR)/.debug/termio.log" --log-level debug --force

clean:
	rm -rf bin dist tmp
