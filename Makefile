BIN ?= $(HOME)/.local/bin/cc2

.PHONY: build install clean

build:
	go build -o cc2 .

install:
	go build -o "$(BIN)" .
	@echo "installed -> $(BIN)"
	@echo "确保 $(dir $(BIN)) 在 PATH 中即可直接使用 cc2"

clean:
	rm -f cc2
