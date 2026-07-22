BIN ?= $(HOME)/.local/bin/cc2
VERSION ?= 0.1.0
PLATFORMS = darwin-arm64 darwin-amd64 linux-amd64 linux-arm64

.PHONY: build install clean release

build:
	go build -o cc2 .

install:
	go build -o "$(BIN)" .
	@echo "installed -> $(BIN)"
	@echo "确保 $(dir $(BIN)) 在 PATH 中即可直接使用 cc2"

# 交叉编译各平台二进制到 dist/, 并生成 SHA256SUMS
release:
	rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
	  os=$${p%-*}; arch=$${p#*-}; \
	  echo "build dist/cc2-$$p"; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/cc2-$$p . ; \
	done
	cd dist && shasum -a 256 cc2-* | tee SHA256SUMS

clean:
	rm -f cc2
	rm -rf dist
