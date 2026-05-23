BIN := dist/whatskept
PKG := ./cmd/whatskept
VERSION ?= 0.0.0-dev

.PHONY: build run list extract app clean tidy fmt vet test

build: $(BIN)

$(BIN): $(shell find . \( -name '*.go' -o -name '*.html' -o -name '*.js' -o -path './internal/helpers/bundle/*' \) -not -path './dist/*' -not -path './.git/*')
	@mkdir -p dist
	go build -ldflags "-X main.Version=$(VERSION)" -o $(BIN) $(PKG)
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
