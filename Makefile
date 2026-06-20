BIN            := dist/whatskept
WINBIN         := dist/whatskept.exe
APP            := dist/WhatsKept.app
PKG            := ./cmd/whatskept
VERSION        ?= 0.0.0-dev

# WIN_SHARE: path (Mac side) that the Windows VM can see, where
# `make build-windows` drops the freshly built .exe so the VM picks it up
# with no extra step. Defaults to ~/Downloads (visible from Parallels via the
# shared home folder). Override with `make build-windows WIN_SHARE=...`, or
# set it empty to build into dist/ only.
WIN_SHARE      ?= $(HOME)/Downloads

# Build tags enabled in dist/whatskept:
#   sqlite_fts5  — mattn/go-sqlite3 ships the FTS5 amalgamation but
#                  leaves it OFF by default. Our views.sql declares
#                  `CREATE VIRTUAL TABLE messages_fts USING fts5(...)`
#                  which fails with `no such module: fts5` without
#                  this tag. Required for the Database tab's sync.
GO_TAGS := sqlite_fts5

.PHONY: build build-windows bundle run list extract app clean tidy fmt vet test

build: $(BIN)

# winicon regenerates the embedded Windows app-icon resource from the source
# logo. Run it after changing internal/app/web/logo.png. Needs ImageMagick +
# mingw-w64 (brew install imagemagick mingw-w64). The resulting .syso is
# committed and auto-linked by `go build` for windows/amd64 (the
# _windows_amd64 filename suffix scopes it to Windows), so build-windows and CI
# don't need windres themselves.
.PHONY: winicon
winicon:
	magick internal/app/web/logo.png -background none \
	  -define icon:auto-resize=256,128,64,48,32,16 build/whatskept.ico
	cd build && x86_64-w64-mingw32-windres -i icon.rc -O coff \
	  -o ../cmd/whatskept/rsrc_windows_amd64.syso
	@echo "regenerated cmd/whatskept/rsrc_windows_amd64.syso"

# build-windows cross-compiles the Windows x64 .exe from this Mac for the
# Parallels test loop — no native build inside the VM required.
#
# Needs the mingw-w64 cross toolchain once:  brew install mingw-w64
# webview_go vendors the WebView2 headers and links only standard Win32
# system libs, so there is no Windows SDK dependency. The macOS-only
# build/sign.sh step is intentionally skipped (Authenticode is a separate
# concern; SmartScreen just warns on first run).
#
# Usage:
#   make build-windows                       # -> dist/whatskept.exe
#   make build-windows WIN_SHARE=~/win-share # also copies into the share
build-windows:
	@mkdir -p dist
	GOOS=windows GOARCH=amd64 CGO_ENABLED=1 \
	  CC=x86_64-w64-mingw32-gcc CXX=x86_64-w64-mingw32-g++ \
	  go build -tags "$(GO_TAGS)" -ldflags "-X main.Version=$(VERSION)" -o $(WINBIN) $(PKG)
	@echo "built $(WINBIN) ($$(du -h $(WINBIN) | cut -f1))"
	@if [ -n "$(WIN_SHARE)" ]; then \
	  mkdir -p "$(WIN_SHARE)"; \
	  cp $(WINBIN) "$(WIN_SHARE)/whatskept.exe"; \
	  echo "copied -> $(WIN_SHARE)/whatskept.exe"; \
	fi

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
