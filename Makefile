BIN            := dist/whatskept
APP            := dist/WhatsKept.app
PKG            := ./cmd/whatskept
VISION_HELPER  := internal/helpers/bundle/whatskept-vision
VISION_SRC     := build/vision-helper/main.swift
FACES_HELPER   := internal/helpers/bundle/whatskept-faces
FACES_SRC      := build/faces-helper/main.swift
WHISPER_HELPER := internal/helpers/bundle/whisper-cli
VERSION        ?= 0.0.0-dev

# Build tags enabled in dist/whatskept:
#   sqlite_fts5  — mattn/go-sqlite3 ships the FTS5 amalgamation but
#                  leaves it OFF by default. Our views.sql declares
#                  `CREATE VIRTUAL TABLE messages_fts USING fts5(...)`
#                  which fails with `no such module: fts5` without
#                  this tag. Required for the Database tab's sync.
GO_TAGS := sqlite_fts5

.PHONY: build bundle vision-helper faces-helper whisper-helper run list extract app clean tidy fmt vet test

build: $(BIN)

# go:embed glob in internal/helpers/embed.go pulls
# internal/helpers/bundle/* into the binary at compile time, so the
# Swift Vision helper must already be built and present in that
# directory before `go build` runs. Listing it as an explicit prereq
# also gives us free incremental rebuilds via Make's file-mtime
# checking: edit main.swift and `make build` rebuilds the helper +
# Go binary; no edits and nothing happens.
$(BIN): vision-helper faces-helper $(shell find . \( -name '*.go' -o -name '*.html' -o -name '*.js' -o -path './internal/helpers/bundle/*' \) -not -path './dist/*' -not -path './.git/*')
	@mkdir -p dist
	go build -tags "$(GO_TAGS)" -ldflags "-X main.Version=$(VERSION)" -o $(BIN) $(PKG)
	@chmod +x build/sign.sh
	@./build/sign.sh $(BIN)

# Apple Vision wrapper used by `whatskept media-index`. A ~250-line
# Swift program compiled to a single ad-hoc-signed arm64 Mach-O that
# whatskept spawns as a persistent subprocess, talking JSON over
# stdin/stdout. See build/vision-helper/main.swift for the protocol.
#
# Building requires Xcode Command Line Tools (`xcode-select --install`),
# which is already a prerequisite for `cgo`-using parts of mattn/go-sqlite3.
# The compiled binary is committed to the repo so a fresh clone can
# `make build` without Swift being touched until you edit the .swift.
vision-helper: $(VISION_HELPER)

$(VISION_HELPER): $(VISION_SRC)
	@command -v swiftc >/dev/null || { echo "swiftc not found — install Xcode CLT: xcode-select --install"; exit 1; }
	@mkdir -p $(dir $(VISION_HELPER))
	swiftc -O -o $(VISION_HELPER) $(VISION_SRC)
	@chmod +x build/sign.sh
	@./build/sign.sh $(VISION_HELPER)

# Apple Vision face detector + clusterer used by the "Find people" card.
# A standalone Swift binary (not the persistent vision helper) that walks
# the workspace media/ folder, detects + embeds + clusters faces across
# all cores in one shot, and writes faces/clusters.json + crop thumbnails.
# Like the vision helper, the compiled binary is committed so a fresh
# clone can `make build` without touching Swift until you edit the .swift.
faces-helper: $(FACES_HELPER)

$(FACES_HELPER): $(FACES_SRC)
	@command -v swiftc >/dev/null || { echo "swiftc not found — install Xcode CLT: xcode-select --install"; exit 1; }
	@mkdir -p $(dir $(FACES_HELPER))
	swiftc -O -o $(FACES_HELPER) $(FACES_SRC)
	@chmod +x build/sign.sh
	@./build/sign.sh $(FACES_HELPER)

# whisper-helper rebuilds internal/helpers/bundle/whisper-cli from
# a fresh checkout of ggerganov/whisper.cpp. The binary is committed
# to the repo (just like the vision helper), so a normal `make build`
# does NOT trigger this — it's a maintainer/CI target you invoke
# explicitly when bumping the upstream whisper.cpp ref or rebuilding
# from a fresh clone. Building requires cmake + a C++ compiler.
#
# The companion model file (~574 MB) is NOT bundled; it's downloaded
# at runtime on first voice-index use. See internal/helpers/model.go.
whisper-helper:
	@command -v cmake >/dev/null || { echo "cmake not found — brew install cmake"; exit 1; }
	@chmod +x build/bundle-whisper.sh
	./build/bundle-whisper.sh

# Always re-sign, even on a no-op build. On Apple Silicon, ANY filesystem-side
# byte modification of a Go-linker-signed Mach-O after build (including some
# macOS provenance-tracking actions) silently invalidates the signature and
# the kernel will refuse to launch it (process freezes in `U` state, 0 CPU,
# unkillable). Re-signing regenerates the signature over current bytes.
.PHONY: sign
sign:
	@chmod +x build/sign.sh
	@./build/sign.sh $(BIN)

# `bundle` wraps dist/whatskept in a proper macOS .app so window
# managers (aerospace, Dock, Mission Control), Spotlight, and the
# OS "Get Info" dialog see a real CFBundleIdentifier instead of
# "NULL-APP-BUNDLE-ID". The bundled binary is the SAME executable;
# main.go detects launch-as-bundle by inspecting os.Args[0] and
# defaults to the GUI subcommand in that case. Idempotent;
# re-running over an existing $(APP) wipes and rebuilds.
bundle: $(APP)

$(APP): $(BIN) build/make-bundle.sh
	@chmod +x build/make-bundle.sh
	@VERSION=$(VERSION) ./build/make-bundle.sh $(BIN)

run: build
	$(BIN)

list: build
	$(BIN) list

extract: build
	$(BIN) extract

app: build
	$(BIN) app

tidy:
	go mod tidy

fmt:
	gofmt -w .

vet:
	go vet ./...

test:
	go test ./...

clean:
	rm -rf dist
