PLUGIN  := pggo
VERSION := 1.0.1
GOOS    ?= $(shell go env GOOS)
GOARCH  ?= $(shell go env GOARCH)

ifeq ($(GOOS),windows)
EXT := dll
else ifeq ($(GOOS),darwin)
EXT := dylib
else
EXT := so
endif

OUT := bin/$(PLUGIN)-v$(VERSION).$(EXT)

.PHONY: build test install clean

# c-shared 必须用目标平台的 C 工具链编译，不能纯 Go 交叉编译。
# 在 CPA 所在机器上直接 make build 最省事。
build:
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -ldflags "-s -w" -o $(OUT) .

test:
	CGO_ENABLED=0 go test -count=1 ./...

# 安装到 CPA 的 plugins/<goos>/<goarch>/ 目录
install: build
	mkdir -p ../plugins/$(GOOS)/$(GOARCH)
	cp $(OUT) ../plugins/$(GOOS)/$(GOARCH)/

clean:
	rm -rf bin
