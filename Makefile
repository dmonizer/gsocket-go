# gsocket-go Makefile
# Cross-compiles the gs-netcat binary for multiple platforms and architectures.

APP     := gs-netcat
CMD     := ./cmd/gs-netcat
OUTDIR  := build
LDFLAGS := -s -w

# Detect current platform for the default target.
CUR_OS   := $(shell go env GOOS)
CUR_ARCH := $(shell go env GOARCH)

# go-legacy-win7 for Windows 7/8 compatibility (oldwin target).
# Fork of Go with patches for legacy Windows; same major version as system Go.
# Auto-downloaded on first use. Override: make oldwin GO_LEGACY=/path/to/go
GO_LEGACY_DIR := $(HOME)/sdk/go-legacy-win7
GO_LEGACY     := $(GO_LEGACY_DIR)/bin/go
GO_LEGACY_VER := v1.26.4-1
GO_LEGACY_TAR := go-legacy-win7-1.26.4-1.linux_amd64.tar.gz
GO_LEGACY_URL := https://github.com/thongtech/go-legacy-win7/releases/download/$(GO_LEGACY_VER)/$(GO_LEGACY_TAR)

# --- Platform / Architecture matrices ---
LINUX_ARCHS   := amd64 arm64 386 arm
WINDOWS_ARCHS := amd64 386
DARWIN_ARCHS  := amd64 arm64

# Arm variants (GOARM)
ARM_VARIANTS := 6 7

# --- Default target: build for current machine ---
.PHONY: default
default:
	@mkdir -p $(OUTDIR)/$(CUR_OS)
	CGO_ENABLED=0 GOOS=$(CUR_OS) GOARCH=$(CUR_ARCH) \
		go build -ldflags="$(LDFLAGS)" -o $(OUTDIR)/$(CUR_OS)/$(APP)-$(CUR_ARCH)$(if $(filter windows,$(CUR_OS)),.exe,) $(CMD)
	@echo "→ $(OUTDIR)/$(CUR_OS)/$(APP)-$(CUR_ARCH)$(if $(filter windows,$(CUR_OS)),.exe,)"

# Keep the old 'build' alias for compatibility.
.PHONY: build
build: default

# --- Platform batch targets ---
.PHONY: linux
linux:
	@mkdir -p $(OUTDIR)/linux
	@for arch in $(LINUX_ARCHS); do \
		if [ "$$arch" = "arm" ]; then \
			for armver in $(ARM_VARIANTS); do \
				echo "Building linux/arm/v$$armver..."; \
				CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=$$armver \
					go build -ldflags="$(LDFLAGS)" -o $(OUTDIR)/linux/$(APP)-armv$$armver $(CMD); \
				echo "  → $(OUTDIR)/linux/$(APP)-armv$$armver"; \
			done; \
		else \
			echo "Building linux/$$arch..."; \
			CGO_ENABLED=0 GOOS=linux GOARCH=$$arch \
				go build -ldflags="$(LDFLAGS)" -o $(OUTDIR)/linux/$(APP)-$$arch $(CMD); \
			echo "  → $(OUTDIR)/linux/$(APP)-$$arch"; \
		fi; \
	done

.PHONY: windows
windows:
	@mkdir -p $(OUTDIR)/windows
	@for arch in $(WINDOWS_ARCHS); do \
		echo "Building windows/$$arch..."; \
		CGO_ENABLED=0 GOOS=windows GOARCH=$$arch \
			go build -ldflags="$(LDFLAGS)" -o $(OUTDIR)/windows/$(APP)-$$arch.exe $(CMD); \
		echo "  → $(OUTDIR)/windows/$(APP)-$$arch.exe"; \
	done

.PHONY: oldwin
oldwin:
	@if [ ! -x "$(GO_LEGACY)" ]; then \
		echo "go-legacy-win7 not found — downloading $(GO_LEGACY_VER)..."; \
		mkdir -p "$$(dirname $(GO_LEGACY_DIR))"; \
		curl -sL "$(GO_LEGACY_URL)" -o /tmp/$(GO_LEGACY_TAR); \
		tar -C "$$(dirname $(GO_LEGACY_DIR))" -xzf /tmp/$(GO_LEGACY_TAR); \
		rm /tmp/$(GO_LEGACY_TAR); \
		echo "Installed: $$($(GO_LEGACY) version)"; \
	fi
	@mkdir -p $(OUTDIR)/windows
	@for arch in $(WINDOWS_ARCHS); do \
		echo "Building windows/$$arch (legacy, Win7 compat)..."; \
		CGO_ENABLED=0 GOOS=windows GOARCH=$$arch \
			$(GO_LEGACY) build -ldflags="$(LDFLAGS)" -o $(OUTDIR)/windows/$(APP)-$$arch-oldwin.exe $(CMD); \
		echo "  → $(OUTDIR)/windows/$(APP)-$$arch-oldwin.exe (Win7+)"; \
	done
	@echo ""
	@echo "Windows 7/8 compatible binaries in $(OUTDIR)/windows/"

.PHONY: macos darwin
macos: darwin
darwin:
	@mkdir -p $(OUTDIR)/darwin
	@for arch in $(DARWIN_ARCHS); do \
		echo "Building darwin/$$arch..."; \
		CGO_ENABLED=0 GOOS=darwin GOARCH=$$arch \
			go build -ldflags="$(LDFLAGS)" -o $(OUTDIR)/darwin/$(APP)-$$arch $(CMD); \
		echo "  → $(OUTDIR)/darwin/$(APP)-$$arch"; \
	done

# --- Build everything ---
.PHONY: all
all: linux windows darwin oldwin
	@echo ""
	@echo "All builds complete:"
	@find $(OUTDIR) -type f | sort | while read f; do \
		printf "  %-55s %s\n" "$$f" "$$(du -h "$$f" | cut -f1)"; \
	done

# --- Test ---
.PHONY: test
test:
	go test -v -count=1 -timeout 30s ./gsocket/...

# --- Vet ---
.PHONY: vet
vet:
	go vet ./...

# --- Clean ---
.PHONY: clean
clean:
	rm -rf $(OUTDIR)

# --- Verify: report cross-compilation targets supported by Go ---
.PHONY: targets
targets:
	@echo "Supported GOOS/GOARCH combinations:"
	@go tool dist list | sort

# --- Help ---
.PHONY: help
help:
	@echo "gsocket-go Makefile"
	@echo ""
	@echo "Targets:"
	@echo "  make             build for current OS/arch  (→ $(OUTDIR)/$(CUR_OS)/)"
	@echo "  make build       alias for default"
	@echo "  make all         build all platforms + archs (→ $(OUTDIR)/)"
	@echo "  make linux       build all Linux variants    (→ $(OUTDIR)/linux/)"
	@echo "  make windows     build all Windows variants  (→ $(OUTDIR)/windows/)"
	@echo "  make oldwin      build Windows with legacy   (Win7/8 compat, auto-download)"
	@echo "  make macos       build all macOS variants    (→ $(OUTDIR)/darwin/)"
	@echo "  make test        run unit tests"
	@echo "  make vet         static analysis"
	@echo "  make clean       remove build artifacts"
	@echo "  make targets     list all Go cross-compile targets"
	@echo "  make help        show this help"
	@echo ""
	@echo "Output files:"
	@echo "  Linux:   gs-netcat-<arch>, gs-netcat-armv6, gs-netcat-armv7"
	@echo "  Windows: gs-netcat-<arch>.exe, gs-netcat-<arch>-oldwin.exe (Win7+)"
	@echo "  macOS:   gs-netcat-<arch>"
	@echo ""
	@echo "Current platform: $(CUR_OS)/$(CUR_ARCH)"
