# ------------------------------------------------------------------------------
# diskbench — Makefile
# Цели: build (динамический), static (статический, musl-safe), docker, run,
#       test, vet, fmt, clean, help
# ------------------------------------------------------------------------------

BINARY      ?= diskbench
PKG         := ./diskbench
IMAGE       ?= diskbench:latest
GO          ?= go
GOFLAGS     ?= -trimpath
LDFLAGS     := -s -w

# Статическая сборка: без cgo + CGO_ENABLED=0 + ldflags '-extldflags "-static"'.
STATIC_ENV  := CGO_ENABLED=0 GOOS=linux GOARCH=amd64

.PHONY: all build static native docker docker-push run test vet fmt clean help

all: build

## build: собрать бинарник в bin/ (обычная сборка под текущую платформу)
build:
	@mkdir -p bin
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BINARY) $(PKG)
	@echo "built: bin/$(BINARY)"

## static: собрать полностью статический бинарник (libc-free, для alpine/docker)
static:
	@mkdir -p bin
	$(STATIC_ENV) $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS) -extldflags "-static"' -o bin/$(BINARY)-static $(PKG)
	@echo "built: bin/$(BINARY)-static"
	@file bin/$(BINARY)-static || true

## native: статический бинарник под текущую архитектуру (без захардкоженного amd64)
native:
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BINARY)-static $(PKG)
	@echo "built: bin/$(BINARY)-static ($(shell $(GO) env GOOS)/$(shell $(GO) env GOARCH))"

## docker: построить multi-stage образ на alpine:latest со статическим бинарником
docker:
	docker build -t $(IMAGE) .
	@echo "image: $(IMAGE)"

## docker-push: построить и запушить образ (нужен IMAGE=registry/repo:tag)
docker-push: docker
	docker push $(IMAGE)

## run: быстрый локальный прогон в /tmp
run: build
	./bin/$(BINARY) -dir /tmp -size 256M -duration 5s

## test: гонки unit-тестов нет, проверяем что проект собирается и проходит vet
test: vet
	$(GO) build $(PKG) -o /dev/null
	@echo "OK"

## vet: статический анализ кода
vet:
	$(GO) vet $(PKG)

## fmt: форматирование исходников
fmt:
	$(GO) fmt $(PKG)

## clean: удалить артефакты сборки
clean:
	rm -rf bin
	rm -f $(BINARY)

## help: список целей
help:
	@grep -E '^## ' Makefile | sed -e 's/^## //' -e 's/: /  —  /' -e 's/^/  make /'
