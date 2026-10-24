// diskbench — бенчмарк дисковой подсистемы на чистом Go (только stdlib).
//
// Измеряет:
//   - IOPS (ops/sec) для произвольных чтений/записей
//   - задержки IO (latency): mean / p50 / p90 / p95 / p99 / max
//   - скорость (MB/s) последовательного и произвольного чтения/записи
//
// Реализация: прямые системные вызовы pread(2)/pwrite(2)/fdatasync(2)
// через пакет syscall, без сторонних зависимостей.
//
// Примеры:
//
//	go run ./diskbench
//	go run ./diskbench -dir /mnt/nvme -size 2G -duration 15s -workers 8
//	go run ./diskbench -bs-rand 4k -bs-seq 1m -fsync=false
package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// ---------------- флаги ----------------

var (
	dirFlag     = flag.String("dir", ".", "директория, в которой создаётся тестовый файл")
	sizeFlag    = flag.String("size", "256M", "размер тестового файла (100M, 1G, ...)")
	durFlag     = flag.Duration("duration", 5*time.Second, "длительность каждой фазы")
	workersFlag = flag.Int("workers", runtime.NumCPU(), "число параллельных потоков I/O")
	bsRandFlag  = flag.String("bs-rand", "4K", "размер блока для random/mixed I/O")
	bsSeqFlag   = flag.String("bs-seq", "1M", "размер блока для sequential I/O")
	fsyncFlag   = flag.Bool("fsync", true, "вызывать fdatasync после каждой записи (sync write)")
	keepFlag    = flag.Bool("no-cleanup", false, "не удалять тестовый файл после прогона")
	rwFlag      = flag.Float64("rw-ratio", 0.7, "доля чтений в mixed random тесте (0..1)")
	colorFlag   = flag.String("color", "auto", "раскраска вывода: auto|always|never")
)

// ---------------- утилиты ----------------

func parseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return 0, fmt.Errorf("пустое значение размера")
	}
	m := int64(1)
	last := s[len(s)-1]
	switch last {
	case 'K':
		m = 1 << 10
		s = s[:len(s)-1]
	case 'M':
		m = 1 << 20
		s = s[:len(s)-1]
	case 'G':
		m = 1 << 30
		s = s[:len(s)-1]
	case 'T':
		m = 1 << 40
		s = s[:len(s)-1]
	case 'B':
		s = s[:len(s)-1]
	}
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
		return 0, fmt.Errorf("некорректный размер %q: %w", s, err)
	}
	v := int64(f * float64(m))
	if v <= 0 {
		return 0, fmt.Errorf("некорректный размер %q", s)
	}
	return v, nil
}

func humanize(v int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	f := float64(v)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%dB", v)
	}
	return fmt.Sprintf("%.1f%s", f, units[i])
}

const align = 4096

// alignedSlice возвращает срез длиной size, начало которого выровнено по границе страницы.
// Возвращает также исходный буфер (для предотвращения GC-смещения использовать не нужно —
// срез держит ссылку на базовый массив).
func alignedSlice(size int) []byte {
	raw := make([]byte, size+align)
	off := int(uintptr(unsafe.Pointer(&raw[0])) % align)
	shift := (align - off) % align
	return raw[shift : shift+size]
}

// pread/pwrite поверх syscall.SYS_PREAD64/SYS_PWRITE64, работающие с обычным []byte без cgo.
func pread(fd int, b []byte, off int64) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	n, _, errno := syscall.Syscall6(syscall.SYS_PREAD64,
		uintptr(fd), uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		uintptr(off), 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(n), nil
}

func pwrite(fd int, b []byte, off int64) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	n, _, errno := syscall.Syscall6(syscall.SYS_PWRITE64,
		uintptr(fd), uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		uintptr(off), 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(n), nil
}

// полные чтение/запись с дозапросом коротких операций
func preadFull(fd int, b []byte, off int64) error {
	done := 0
	for done < len(b) {
		n, err := pread(fd, b[done:], off+int64(done))
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
		done += n
	}
	return nil
}

func pwriteFull(fd int, b []byte, off int64) error {
	done := 0
	for done < len(b) {
		n, err := pwrite(fd, b[done:], off+int64(done))
		if err != nil {
			return err
		}
		if n == 0 {
			return syscall.ENOSPC
		}
		done += n
	}
	return nil
}

func fdatasync(fd int) error {
	_, _, errno := syscall.Syscall(syscall.SYS_FDATASYNC, uintptr(fd), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// ---------------- статистика задержек ----------------

type latStats struct {
	mean, p50, p90, p95, p99, max time.Duration
}

func statsOf(lat []int64) latStats {
	if len(lat) == 0 {
		return latStats{}
	}
	cp := append([]int64(nil), lat...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	var sum int64
	for _, v := range cp {
		sum += v
	}
	pct := func(p float64) time.Duration {
		idx := int(math.Ceil(p/100*float64(len(cp)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(cp) {
			idx = len(cp) - 1
		}
		return time.Duration(cp[idx])
	}
	return latStats{
		mean: time.Duration(sum / int64(len(cp))),
		p50:  pct(50),
		p90:  pct(90),
		p95:  pct(95),
		p99:  pct(99),
		max:  time.Duration(cp[len(cp)-1]),
	}
}

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Microsecond:
		return fmt.Sprintf("%.0fns", float64(d)/float64(time.Nanosecond))
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fus", float64(d)/float64(time.Microsecond))
	default:
		return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
	}
}

// ---------------- результат фазы ----------------

type result struct {
	name    string
	ops     int64
	bytes   int64
	lat     []int64
	seconds float64
	err     error
}

func (r result) iops() float64 { return float64(r.ops) / r.seconds }
func (r result) mbps() float64 { return float64(r.bytes) / r.seconds / (1 << 20) }

// ---------------- движок теста ----------------

// runPhase запускает workers горутин, каждая выполняет IO-операции до дедлайна.
// mode: "seq" | "rand" | "mixed"; isWrite — режим для seq/rand; для mixed доля
// чтений задаётся rwRatio.
func runPhase(name string, fd int, fileSz int64, bs int, workers int,
	dur time.Duration, mode string, isWrite bool, rwRatio float64) result {

	res := result{name: name}
	if workers < 1 {
		workers = 1
	}

	var totalOps, totalBytes int64
	allLat := make([][]int64, workers)
	var mu sync.Mutex // защита res.err
	var wg sync.WaitGroup
	deadline := time.Now().Add(dur)
	start := time.Now()

	nblocks := fileSz / int64(bs)
	if nblocks < int64(workers) {
		nblocks = int64(workers)
		fileSz = nblocks * int64(bs)
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()

			buf := alignedSlice(bs)
			for i := range buf {
				buf[i] = byte(i)
			}

			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(w)*7919))
			// каждому воркеру — своя непересекающаяся полоса блоков,
			// чтобы sequential-потоки не конфликтовали
			base := int64(w) * nblocks / int64(workers)
			end := int64(w+1) * nblocks / int64(workers)
			if end <= base {
				end = base + 1
			}
			cnt := end - base

			lat := make([]int64, 0, 8192)
			var ops, bytes int64
			seqIdx := rng.Int63n(cnt)

			for {
				var off int64
				var write bool
				switch mode {
				case "seq":
					off = (base + seqIdx%cnt) * int64(bs)
					seqIdx++
					write = isWrite
				case "rand":
					off = (base + rng.Int63n(cnt)) * int64(bs)
					write = isWrite
				case "mixed":
					off = (base + rng.Int63n(cnt)) * int64(bs)
					write = rng.Float64() >= rwRatio
				}
				if off+int64(bs) > fileSz {
					off = fileSz - int64(bs)
					if off < 0 {
						off = 0
					}
				}

				t0 := time.Now()
				var err error
				if write {
					err = pwriteFull(fd, buf, off)
					if err == nil && *fsyncFlag {
						err = fdatasync(fd)
					}
				} else {
					err = preadFull(fd, buf, off)
				}
				el := time.Since(t0).Nanoseconds()

				if err != nil {
					mu.Lock()
					if res.err == nil {
						res.err = fmt.Errorf("%s: worker %d: %w", name, w, err)
					}
					mu.Unlock()
					break
				}
				lat = append(lat, el)
				ops++
				bytes += int64(bs)

				if ops%512 == 0 && time.Now().After(deadline) {
					break
				}
			}
			allLat[w] = lat
			atomic.AddInt64(&totalOps, ops)
			atomic.AddInt64(&totalBytes, bytes)
		}(w)
	}
	wg.Wait()

	el := time.Since(start).Seconds()
	if el <= 0 {
		el = 1e-9
	}
	res.ops = totalOps
	res.bytes = totalBytes
	res.seconds = el
	var merged []int64
	for _, l := range allLat {
		merged = append(merged, l...)
	}
	res.lat = merged
	return res
}

// ---------------- подготовка файла ----------------

func prepareFile(path string, size int64) (int, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_TRUNC, 0o644)
	if err != nil {
		return -1, err
	}
	if err := syscall.Ftruncate(fd, size); err != nil {
		syscall.Close(fd)
		return -1, err
	}
	// пред-заполняем файл данными, чтобы random read бил по реальным блокам
	buf := alignedSlice(1 << 20)
	chunk := int64(len(buf))
	for off := int64(0); off < size; off += chunk {
		n := chunk
		if size-off < n {
			n = size - off
		}
		if _, err := pwrite(fd, buf[:n], off); err != nil {
			syscall.Close(fd)
			return -1, err
		}
	}
	if err := syscall.Fsync(fd); err != nil {
		syscall.Close(fd)
		return -1, err
	}
	// сбрасываем page cache файла (posix_fadvise DONTNEED), чтобы read-фазы
	// били по диску, а не по кэшу ядра. Ошибку игнорируем — fadvise носит
	// рекомендательный характер.
	dropCache(fd, size)
	return fd, nil
}

// dropCache выбрасывает страницы файла из page cache через posix_fadvise(POSIX_FADV_DONTNEED).
func dropCache(fd int, size int64) {
	const POSIX_FADV_DONTNEED = 6
	syscall.Syscall6(syscall.SYS_FADVISE64, uintptr(fd), 0, uintptr(size), POSIX_FADV_DONTNEED, 0, 0)
}

// ---------------- вывод ----------------

// ANSI-коды (без внешних зависимостей, только stdlib).
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiBlue   = "\x1b[34m"
	ansiCyan   = "\x1b[36m"
)

// colorEnabled определяется в main(): auto — только если stdout это TTY.
var colorEnabled bool

// isTTY проверяет, что файловый дескриптор привязан к терминалу
// (символьное устройство), без сторонних библиотек.
func isTTY(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// col оборачивает текст в ANSI-код, если раскраска включена.
func col(code, s string) string {
	if !colorEnabled {
		return s
	}
	return code + s + ansiReset
}

// pad форматирует v по шаблону format и дополняет пробелами слева до ширины w.
func pad(v float64, w int, format string) string {
	s := fmt.Sprintf(format, v)
	for len(s) < w {
		s = " " + s
	}
	return s
}

// latColor — цвет для значения задержки: быстрый отклик — зелёный,
// средний — жёлтый, медленный — красный.
func latColor(d time.Duration) string {
	switch {
	case d < 500*time.Microsecond:
		return ansiGreen
	case d < 5*time.Millisecond:
		return ansiYellow
	default:
		return ansiRed
	}
}

// iopsColor — чем больше IOPS, тем «зеленее».
func iopsColor(v float64) string {
	switch {
	case v >= 100_000:
		return ansiGreen
	case v >= 10_000:
		return ansiYellow
	default:
		return ansiRed
	}
}

// mbpsColor — аналогично для пропускной способности.
func mbpsColor(v float64) string {
	switch {
	case v >= 1000:
		return ansiGreen
	case v >= 200:
		return ansiYellow
	default:
		return ansiRed
	}
}

// phaseColor возвращает «акцентный» цвет названия фазы:
// запись — синий, чтение — циан, смешанная — жёлтый.
func phaseColor(name string) string {
	switch {
	case strings.Contains(name, "write"):
		return ansiBlue
	case strings.Contains(name, "read"):
		return ansiCyan
	default:
		return ansiYellow
	}
}

func header() {
	sep := col(ansiDim, "|")
	fmt.Printf("%s %s %s %s %s %s %s %s %s\n",
		col(ansiBold, fmt.Sprintf("%-16s", "TEST")), sep,
		col(ansiBold, fmt.Sprintf("%10s", "IOPS")), sep,
		col(ansiBold, fmt.Sprintf("%9s", "MB/s")), sep,
		col(ansiBold, fmt.Sprintf("%8s", "LAT avg")), sep,
		col(ansiBold, "p50 / p90 / p95 / p99 / max"))
	fmt.Println(col(ansiDim, strings.Repeat("-", 104)))
}

func printRow(r result) {
	st := statsOf(r.lat)
	sep := col(ansiDim, "|")

	nameStr := col(ansiBold+phaseColor(r.name), fmt.Sprintf("%-16s", r.name))
	iopsStr := col(iopsColor(r.iops()), pad(r.iops(), 10, "%.0f"))
	mbpsStr := col(mbpsColor(r.mbps()), pad(r.mbps(), 9, "%.1f"))

	// LAT avg: красим видимый текст, выравнивание (правая колонка ширины 8)
	// добавляем пробелами вне цвета, чтобы escape-коды не ломали сетку таблицы.
	avgText := fmtDur(st.mean)
	padAvg := ""
	for len(avgText)+len(padAvg) < 8 {
		padAvg = " " + padAvg
	}
	avgStr := col(latColor(st.mean), avgText) + padAvg

	lats := []time.Duration{st.p50, st.p90, st.p95, st.p99, st.max}
	var sb strings.Builder
	for i, d := range lats {
		sb.WriteString(col(latColor(d), fmtDur(d)))
		if i < len(lats)-1 {
			sb.WriteString(" ")
		}
	}

	fmt.Printf("%s %s %s %s %s %s %s %s %s\n",
		nameStr, sep, iopsStr, sep, mbpsStr, sep, avgStr, sep, sb.String())
	if r.err != nil {
		fmt.Printf("%-16s %s %v\n", "", col(ansiRed, "!"), r.err)
	}
}

// ---------------- main ----------------

func main() {
	flag.Parse()

	// раскраска: auto — только если stdout это TTY и не перенаправлен в файл/pipe
	switch *colorFlag {
	case "always":
		colorEnabled = true
	case "never":
		colorEnabled = false
	default: // "auto"
		colorEnabled = isTTY(os.Stdout)
		if v, ok := os.LookupEnv("NO_COLOR"); ok && v != "" {
			colorEnabled = false
		}
	}

	fileSize, err := parseSize(*sizeFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
	bsRand, err := parseSize(*bsRandFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
	bsSeq, err := parseSize(*bsSeqFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
	// кратность размеру страницы — корректно для выравнивания и честных сравнений
	if bsRand < align {
		bsRand = align
	}
	bsRand = bsRand / align * align
	if bsSeq < align {
		bsSeq = align
	}
	bsSeq = bsSeq / align * align

	testPath := filepath.Join(*dirFlag, "diskbench.testfile")
	fmt.Printf("%s | dir=%s file=%s size=%s duration=%s workers=%d bs_rand=%s bs_seq=%s fsync=%v\n",
		col(ansiBold, "diskbench"), *dirFlag, testPath, humanize(fileSize), *durFlag, *workersFlag,
		humanize(bsRand), humanize(bsSeq), *fsyncFlag)
	fmt.Println()

	fd, err := prepareFile(testPath, fileSize)
	if err != nil {
		fmt.Fprintf(os.Stderr, "не удалось подготовить файл: %v\n", err)
		os.Exit(1)
	}
	defer syscall.Close(fd)
	if !*keepFlag {
		defer os.Remove(testPath)
	}

	header()

	phases := []struct {
		name    string
		mode    string
		bs      int
		isWrite bool
	}{
		{"seq-write", "seq", int(bsSeq), true},
		{"seq-read", "seq", int(bsSeq), false},
		{"rand-write", "rand", int(bsRand), true},
		{"rand-read", "rand", int(bsRand), false},
		{"mixed-rw", "mixed", int(bsRand), false},
	}

	var totOps, totBytes int64
	for _, ph := range phases {
		// перед фазами чтения дополнительно выбрасываем page cache,
		// чтобы предыдущие фазы записи не «подогрели» кэш
		if !ph.isWrite && ph.mode != "mixed" {
			syscall.Fsync(fd)
			dropCache(fd, fileSize)
		}
		r := runPhase(ph.name, fd, fileSize, ph.bs, *workersFlag, *durFlag, ph.mode, ph.isWrite, *rwFlag)
		printRow(r)
		totOps += r.ops
		totBytes += r.bytes
	}

	fmt.Println(col(ansiDim, strings.Repeat("-", 104)))
	fmt.Printf("Всего выполнено операций: %s, данных: %s\n",
		col(ansiBold, fmt.Sprintf("%d", totOps)), humanize(totBytes))
	if *keepFlag {
		fmt.Printf("Тестовый файл сохранён: %s\n", testPath)
	}
}
