# syntax=docker/dockerfile:1
# ------------------------------------------------------------------------------
# diskbench — multi-stage build:
#   stage 1: golang — компиляция СТАТИЧЕСКОГО бинарника (CGO_ENABLED=0)
#   stage 2: alpine:latest — рантайм; бинарник не зависит от libc вообще,
#            поэтому работает даже на чистом alpine (musl) без gcompat.
# ------------------------------------------------------------------------------

# ---------- build stage ----------
FROM golang:1.21-alpine AS builder

WORKDIR /src

# Копируем модульные файлы первыми — слой зависимостей кэшируется отдельно.
COPY go.mod ./
# Зависимости только stdlib — go mod download почти nothing, но держим для кэша.
RUN go mod download

COPY diskbench/ ./diskbench/

# Полностью статическая сборка: без cgo, урезанные символы/отладка.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath \
        -ldflags="-s -w -extldflags \"-static\"" \
        -o /out/diskbench ./diskbench

# ---------- runtime stage ----------
FROM alpine:latest

# Минимум, нужный для работы: ca-certs не требуется, но tini полезен как PID 1.
RUN apk add --no-cache tini

# Статический бинарник — пользовательские UID/GID работают без группы/дома.
COPY --from=builder /out/diskbench /usr/local/bin/diskbench

# Тестовый файл пишем в смонтированный хостом том (-v /host/path:/data).
VOLUME ["/data"]
WORKDIR /data

# Аргументы пробрасываются в diskbench:
#   docker run --rm -v /mnt/nvme:/data diskbench:latest -size 2G -workers 8
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/diskbench", "-dir", "/data"]
CMD ["-size", "256M", "-duration", "5s"]
