VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet dist clean run

build:
	go build -ldflags "$(LDFLAGS)" -o oline ./cmd/oline

run: build
	./oline

test:
	go test ./...

vet:
	go vet ./...

dist: icon
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o oline-linux-amd64      ./cmd/oline
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o oline-windows-amd64.exe ./cmd/oline

# Embed the Windows .exe icon from assets/oline.ico (generates a committed .syso
# the Go linker picks up for Windows builds). Needs the rsrc tool (fetched once).
icon: cmd/oline/rsrc_windows_amd64.syso
cmd/oline/rsrc_windows_amd64.syso: assets/oline.ico
	go run github.com/akavel/rsrc@latest -ico assets/oline.ico -arch amd64 -o $@

clean:
	rm -f oline oline.exe oline-linux-amd64 oline-windows-amd64.exe
