BIN            := dist/whatskept
APP            := dist/WhatsKept.app
PKG            := ./cmd/whatskept
VERSION        ?= 0.0.0-dev

# Build tags enabled in dist/whatskept:
#   sqlite_fts5  — mattn/go-sqlite3 ships the FTS5 amalgamation but
#                  leaves it OFF by default. Our views.sql declares
#                  `CREATE VIRTUAL TABLE messages_fts USING fts5(...)`
#                  which fails with `no such module: fts5` without
#                  this tag. Required for the Database tab's sync.
GO_TAGS := sqlite_fts5

.PHONY: build bundle run list extract app clean tidy fmt vet test

build: $(BIN)

# The go:embed glob in internal/helpers/embed.go pulls
# internal/helpers/bundle/* into the binary at compile time. Those are
# committed third-party binaries (the libimobiledevice iOS-backup tools
# and their dylibs), so a fresh clone can `make build` with no extra
# build step. Listing the bundle as a prereq gives free incremental
# rebuilds via Make's file-mtime checking.
$(BIN): $(shell find . \( -name '*.go' -o -name '*.html' -o -name '*.js' -o -path './internal/helpers/bundle/*' \) -not -path './dist/*' -not -path './.git/*')
	@mkdir -p dist
	go build -tags "$(GO_TAGS)" -ldflags "-X main.Version=$(VERSION)" -o $(BIN) $(PKG)
	@chmod +x build/sign.sh
	@./build/sign.sh $(BIN)

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
