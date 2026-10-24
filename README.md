# diskbench

Бенчмарк дисковой подсистемы на **чистом Go** — используются только пакеты
стандартной библиотеки (`syscall`, `os`, `sync`, `time` и т.д.), без cgo и
сторонних зависимостей.

## Что измеряет

| Метрика | Описание |
|---|---|
| **IOPS** | операций в секунду (ops/sec) |
| **MB/s** | пропускная способность (speed read/write) |
| **Latency** | время отклика IO: avg / p50 / p90 / p95 / p99 / max (io response) |

Фазы теста:

1. `seq-write`  — последовательная запись (bs по умолчанию 1 MiB)
2. `seq-read`   — последовательное чтение
3. `rand-write` — произвольная запись (bs по умолчанию 4 KiB)
4. `rand-read`  — произвольное чтение
5. `mixed-rw`   — смешанная нагрузка random read/write (по умолчанию 70/30)

Реализация: прямые системные вызовы `pread64(2)` / `pwrite64(2)` /
`fdatasync(2)` / `posix_fadvise(DONTNEED)` через `syscall.Syscall6`,
параллельные воркеры с непересекающимися областями файла, сбор гистограммы
задержек каждой операции.

## Запуск

```bash
# дефолтный прогон (текущая директория, файл 256M, 5 сек на фазу)
go run ./diskbench

# NVMe-диск, файл 2G, 8 потоков, 15 секунд на фазу
go run ./diskbench -dir /mnt/nvme -size 2G -duration 15s -workers 8

# async-запись (без fsync после каждого блока), соотношение read/write 50/50
go run ./diskbench -fsync=false -rw-ratio 0.5

# собрать бинарник
go build -o diskbench ./diskbench
```

## Флаги

| Флаг | Тип | По умолчанию | Подробное описание |
|---|---|---|---|
| `-dir` | string | `.` | Директория, в которой создаётся тестовый файл `diskbench.testfile`. Укажите точку монтирования тестируемого устройства (например `/mnt/nvme` или путь на FC-LUN). Каталог должен существовать и быть доступен на запись — diskbench сам каталоги не создаёт. |
| `-size` | string | `256M` | Размер тестового файла; поддерживаются суффиксы `K`, `M`, `G` (`100M`, `4G`, …). Важно: размер должен превышать объём RAM, иначе random-read будет читаться из page cache. Для серьёзных прогонов рекомендуется ≥ 2×RAM. |
| `-duration` | duration | `5s` | Длительность каждой фазы (`10s`, `1m`, …). Чем дольше прогон — тем стабильнее процентили, особенно «хвосты» p99/max. |
| `-workers` | int | число CPU | Количество параллельных потоков I/O. Каждый воркер получает свою непересекающуюся полосу блоков файла. Для HDD оптимум 1–4, для SATA SSD 4–16, для NVMe и FC-SAN — 8–32+. |
| `-bs-rand` | string | `4K` | Размер блока для random/mixed операций. Классика для СУБД — `4K` или `8K`; для файловых нагрузок — `64K`. |
| `-bs-seq` | string | `1M` | Размер блока для sequential операций. `1M` — типичный профиль стриминга/бэкапа; `256K` ближе к профилю СУБД. |
| `-fsync` | bool | `true` | Вызывать `fdatasync(2)` после каждой записи (синхронная запись, как в СУБД). `-fsync=false` измеряет производительность кэшированной (asynchronous) записи — часто упирается в RAM, а не в диск. |
| `-rw-ratio` | float | `0.7` | Доля чтений в фазе `mixed-rw` (0..1). `0.7` = 70% read / 30% write (OLTP-профиль), `0.5` = равная смесь, `1.0`/`0.0` = фактически чистое чтение/запись. |
| `-color` | string | `auto` | Раскраска ANSI-кодами: `auto` (красить только при TTY stdout), `always` (принудительно), `never` (выключить). Учитывается переменная окружения `NO_COLOR`. |
| `-no-cleanup` | bool | `false` | Не удалять тестовый файл после прогона (остаётся в `-dir`). Полезно для повторных прогонов и прогрева тонких LUN. |

---

## Сборка через Makefile

В репозитории лежит `Makefile` со следующими целями (`make help` показывает то же самое):

| Цель | Что делает |
|---|---|
| `make build` | собирает бинарник в `bin/diskbench` под текущую платформу |
| `make static` | собирает **полностью статический** бинарник `bin/diskbench-static` (`CGO_ENABLED=0`, `-extldflags "-static"`) — не зависит от libc, работает на любом Linux, включая Alpine/musl |
| `make native` | статический бинарник под текущую архитектуру (без захардкоженного `GOARCH=amd64`) |
| `make docker` | собирает multi-stage Docker-образ `diskbench:latest` на базе `alpine:latest` |
| `make docker-push` | собирает и пушит образ (передать `IMAGE=registry/repo:tag`) |
| `make run` | быстрый локальный прогон в `/tmp` (файл 256M, 5 сек на фазу) |
| `make test` | `go vet` + проверка компиляции |
| `make vet` / `make fmt` | статанализ / форматирование кода |
| `make clean` | удаляет каталог `bin/` |

Переопределяемые переменные: `BINARY` (имя бинарника), `IMAGE` (имя тега образа), `GO` (путь к go).

```bash
make static                                  # bin/diskbench-static (~1.5 MB, libc-free)
$ ldd bin/diskbench-static
        not a dynamic executable             # подтверждение, что бинарник статический
```

---

## Docker: сборка и запуск

### Устройство Dockerfile

Multi-stage сборка:

1. **builder** (`golang:1.21-alpine`) — компилирует статический бинарник
   (`CGO_ENABLED=0`, `-ldflags "-s -w -extldflags \"-static\""`);
2. **runtime** (`alpine:latest`) — минимальный образ + `tini` как PID 1.
   Статический бинарник не ссылается на libc, поэтому musl-совместимость не нужна.

Тестовый файл пишется в `/data` — туда монтируется тестируемый том.
`ENTRYPOINT` уже содержит `-dir /data`, поэтому аргументы `docker run`
пробрасываются напрямую флагам diskbench; параметры по умолчанию (`CMD`) —
`-size 256M -duration 5s`.

### Сборка образа

```bash
make docker                                   # -> diskbench:latest
# или вручную:
docker build -t diskbench:latest .
# свой реестр/тег:
docker build -t registry.example.com/diskbench:v1 .
make docker-push IMAGE=registry.example.com/diskbench:v1
```

### Запуск контейнера

> ⚠️ Бенчмарку нужен доступ к реальному диску: монтируйте хостовую директорию
> через `-v ...:/data`. Прогон без монтирования измеряет overlayfs контейнера,
> а не ваше устройство.

```bash
# NVMe: примонтировать /mnt/nvme внутрь контейнера как /data
docker run --rm -v /mnt/nvme:/data diskbench:latest \
    -size 4G -duration 30s -workers 16

# HDD, профиль 4K random, однопоточно
docker run --rm -v /mnt/hdd:/data diskbench:latest \
    -size 2G -duration 60s -workers 1 -bs-rand 4K

# FC-LUN: сначала монтировать LUN на хосте, затем прогон в контейнере
sudo mount /dev/mapper/mpath0 /mnt/fcstore
docker run --rm -v /mnt/fcstore:/data diskbench:latest \
    -size 16G -duration 60s -workers 8 -bs-rand 8K

# дефолтный быстрый прогон (параметры из CMD)
docker run --rm -v /tmp/dbtest:/data diskbench:latest

# сохранить результат на хост (цвета выключатся сами — stdout не TTY)
docker run --rm -v /mnt/nvme:/data diskbench:latest \
    -size 8G -duration 30s > nvme-docker-bench.txt
```

Полезные опции запуска:

| Опция | Зачем |
|---|---|
| `--rm` | удалить контейнер после завершения |
| `-v /host/path:/data` | примонтировать тестируемое устройство в рабочую директорию |
| `--privileged` | снимает ограничения cgroup/devices — при тесте блочных устройств напрямую |
| `--device=/dev/nvme0n1` | альтернатива `--privileged`: проброс конкретного устройства |
| `--net=none` | сеть бенчмарку не нужна — изолируем лишнее |
| `-u 0:0` | если UID в контейнере не может писать в монтирование |

---

## Примеры запуска по типам хранилищ

Перед любым прогоном убедитесь, что файловая система смонтирована именно на
нужное устройство (`df -h <путь>`, `lsblk -o NAME,MOUNTPOINT,ROTA,TRAN`), и на
нём нет другой активной нагрузки.

### Локальный HDD

Механический диск медленный на случайных операциях (~100–200 IOPS при 4K),
поэтому: меньше потоков, больше длительность прогона.

```bash
# базовый профиль: 1 поток, 4K random, 30 секунд на фазу, файл 2G
./diskbench -dir /mnt/hdd -size 2G -duration 30s -workers 1 -bs-rand 4K

# многопоточная случайная нагрузка (имитация нескольких клиентов)
./diskbench -dir /mnt/hdd -size 4G -duration 60s -workers 4 -bs-rand 4K -rw-ratio 0.5

# последовательный стриминг (видео/бэкап-профиль)
./diskbench -dir /mnt/hdd -size 8G -duration 20s -workers 1 -bs-seq 1M
```

Ориентиры: rand-IOPS 100–200 (7200 об/мин), seq 100–200 MB/s, задержки
random-запросов 5–15 мс — норма для механики.

### Локальный SATA SSD

SSD хорошо распараллеливается, но канал SATA ограничен ~500–600 MB/s:

```bash
# OLTP-профиль: 8K random, 8 потоков, sync-запись (как у БД с fsync)
./diskbench -dir /mnt/ssd -size 4G -duration 30s -workers 8 -bs-rand 8K

# максимум IOPS: мелкий блок, много потоков
./diskbench -dir /mnt/ssd -size 4G -duration 30s -workers 16 -bs-rand 4K

# кэшированная (async) запись — оценка DRAM-кэша контроллера
./diskbench -dir /mnt/ssd -size 8G -duration 20s -workers 4 -fsync=false
```

Ориентиры: 10k–90k rand-IOPS, seq 400–550 MB/s. После интенсивной записи
дайте SSD 10–15 минут на сборку мусора контроллера перед следующим прогоном.

### NVMe SSD

NVMe раскрывается при глубокой очереди — используйте максимум воркеров и
большие файлы (размер > RAM!):

```bash
# полный стресс: 32 потока, 4K random, файл 16G, минута на фазу
./diskbench -dir /mnt/nvme -size 16G -duration 60s -workers 32 -bs-rand 4K

# смешанная OLTP-нагрузка 70R/30W, sync-запись
./diskbench -dir /mnt/nvme -size 8G -duration 30s -workers 16 -rw-ratio 0.7

# последовательный throughput крупным блоком
./diskbench -dir /mnt/nvme -size 32G -duration 20s -workers 8 -bs-seq 4M
```

Ориентиры: 300k–1M+ rand-IOPS (Gen3/Gen4), seq 2–7 GB/s, p50 — десятки
микросекунд. Если заметно ниже — проверьте настройки энергосбережения диска.

### FC (Fibre Channel) хранилище

Для FC-LUN критично тестировать тот же путь, по которому пойдёт реальная
нагрузка: multipath (dm-multipath), конкретный HBA, очередь на СХД. Перед
прогоном:

```bash
multipath -ll                       # карта путей до LUN
df -h /mnt/fcstore                  # точка монтирования на dm-mapper устройстве
iostat -x 1                         # в соседнем терминале: aqu-sz, %util, await
```

```bash
# консервативный профиль для СХД: умеренный параллелизм, sync-запись
./diskbench -dir /mnt/fcstore -size 16G -duration 60s -workers 8 -bs-rand 8K -fsync=true

# пиковые IOPS СХД (4K, глубокая очередь)
./diskbench -dir /mnt/fcstore -size 32G -duration 60s -workers 32 -bs-rand 4K

# sequential throughput канала FC (бэкапы/клонирование)
./diskbench -dir /mnt/fcstore -size 64G -duration 30s -workers 4 -bs-seq 1M

# OLTP-смесь 70R/30W на FC-SAN
./diskbench -dir /mnt/fcstore -size 16G -duration 60s -workers 16 -bs-rand 8K -rw-ratio 0.7
```

Нюансы FC:
* задержка включает RTT до СХД и обработку контроллером — нормальный p50 для
  FC-SAN ≈ 0.5–2 мс (в разы выше локального NVMe);
* размер файла должен превышать RAM сервера, иначе read-фаза уйдёт в кэш;
* на тонко аллоцированных LUN первый прогон будет медленнее (идёт allocation) —
  делайте 2–3 прогона с `-no-cleanup` и оценивайте второй/третий;
* результат сильно зависит от RAID-уровня и кэша стораджа — фиксируйте
  конфигурацию СХД в протоколе теста.

## Раскраска вывода

Таблица раскрашивается ANSI-кодами (только stdlib, без внешних библиотек):

* **название фазы**: запись — синий, чтение — циан, смешанная — жёлтый;
* **IOPS / MB/s**: зелёный — высокая производительность, жёлтый — средняя, красный — низкая;
* **задержки** (avg и все процентили): зелёный `< 500 мкс`, жёлтый `< 5 мс`, красный иначе;
* разделители таблицы и линия — приглушённый (dim) цвет.

Режим `-color`:
* `auto` (по умолчанию) — цвета только если stdout является терминалом (TTY);
  при перенаправлении в файл/pipe вывод остаётся чистым текстом. Также
  учитывается переменная окружения `NO_COLOR`.
* `always` — принудительно красить (например, для `less -R`).
* `never` — отключить раскраску полностью.

Совет: `go run ./diskbench -color always | less -R` — цвета сохранятся в пейджере.

## Пример вывода

```
diskbench | dir=/tmp/dbtest size=64.0MiB duration=2s workers=4 bs_rand=4.0KiB bs_seq=1.0MiB fsync=true

TEST             |       IOPS |      MB/s |  LAT avg | p50 / p90 / p95 / p99 / max
--------------------------------------------------------------------------------------------------------
seq-write        |       2635 |    2635.2 |   1.45ms | 318.5us 685.5us 2.40ms 35.34ms 195.86ms
seq-read         |       6072 |    6071.9 |   640us  | 163.7us 421.7us 588.5us 7.68ms 62.93ms
rand-write       |     281674 |    1100.3 |   14.0us | 2.6us 4.8us 5.4us 14.8us 68.58ms
rand-read        |     697520 |    2724.7 |    5.3us | 1.2us 1.6us 1.8us 2.2us 108.58ms
mixed-rw         |     439056 |    1715.1 |    8.8us | 1.6us 3.6us 4.4us 8.6us 88.83ms
```

## Замечания

* Тест работает через page cache ядра (как `fio` без `direct=1`). Для чтения
  перед фазами `*-read` кэш файла сбрасывается через `posix_fadvise(DONTNEED)`.
  Если нужен честный direct I/O — запускайте на разделе, где можно
  перемонтировать с `O_DIRECT`, либо сравните результаты с `fio --direct=1`.
* `-fsync=true` включает синхронную запись (типично для БД); `-fsync=false`
  измеряет производительность кэшированной записи.
* Каждый воркер получает свою непересекающуюся полосу блоков файла, поэтому
  sequential-фазы остаются линейными даже при `-workers > 1`.
* Требуется Linux (используются `SYS_PREAD64/SYS_PWRITE64/SYS_FADVISE64`).
