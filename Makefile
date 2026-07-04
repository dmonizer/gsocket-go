# gsocket-go Makefile
# Cross-compiles the gs-netcat binary for multiple platforms and architectures.

APP     := gs-netcat
CMD     := ./cmd/gs-netcat
OUTDIR  := build
LDFLAGS := -s -w

# Detect current platform for the default target.
CUR_OS   := $(shell go env GOOS)
CUR_ARCH := $(shell go env GOARCH)

# Go 1.20 for Windows 7/8 compatibility (oldwin target).
# Go 1.21+ uses APIs unavailable on Windows 7 (GetSystemTimePreciseAsFileTime).
# Override via: make oldwin GO120=/path/to/go1.20
GO120 ?= go1.20

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
	@command -v $(GO120) >/dev/null 2>&1 || { \
		echo "ERROR: Go 1.20 not found (GO120=$(GO120))"; \
		echo ""; \
		echo "Go 1.21+ dropped Windows 7 support. Install Go 1.20:"; \
		echo "  mkdir -p ~/sdk && curl -sL https://go.dev/dl/go1.20.14.linux-amd64.tar.gz | tar -C ~/sdk -xz"; \
		echo "  mv ~/sdk/go ~/sdk/go1.20.14"; \
		echo "  make oldwin GO120=~/sdk/go1.20.14/bin/go"; \
		echo ""; \
		echo "Or install via your package manager and set GO120 to the binary path."; \
		exit 1; \
	}
	@mkdir -p $(OUTDIR)/windows
	@for arch in $(WINDOWS_ARCHS); do \
		echo "Building windows/$$arch (Go 1.20, Win7 compat)..."; \
		CGO_ENABLED=0 GOOS=windows GOARCH=$$arch \
			$(GO120) build -ldflags="$(LDFLAGS)" -o $(OUTDIR)/windows/$(APP)-$$arch.exe $(CMD); \
		echo "  → $(OUTDIR)/windows/$(APP)-$$arch.exe (Win7+)"; \
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
	@echo "  make oldwin      build Windows with Go 1.20  (Win7/8 compat)"
	@echo "  make macos       build all macOS variants    (→ $(OUTDIR)/darwin/)"
	@echo "  make test        run unit tests"
	@echo "  make vet         static analysis"
	@echo "  make clean       remove build artifacts"
	@echo "  make targets     list all Go cross-compile targets"
	@echo "  make help        show this help"
	@echo ""
	@echo "Output files:"
	@echo "  Linux:   gs-netcat-<arch>, gs-netcat-armv6, gs-netcat-armv7"
	@echo "  Windows: gs-netcat-<arch>.exe"
	@echo "  macOS:   gs-netcat-<arch>"
	@echo ""
	@echo "Current platform: $(CUR_OS)/$(CUR_ARCH)"
