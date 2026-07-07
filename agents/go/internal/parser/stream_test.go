package parser

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
)

// collectSplit — эталон: события через SplitEvents (файл целиком в памяти).
func collectSplit(data []byte) []string {
	var got []string
	SplitEvents(data, func(ev []byte) { got = append(got, string(ev)) })
	return got
}

// collectScan — события через потоковый scanEvents с заданным чанком/гвардом.
func collectScan(t *testing.T, data []byte, chunk, guard int) []string {
	t.Helper()
	var got []string
	_, n, err := scanEvents(bytes.NewReader(data), nil, chunk, guard, func(ev []byte) {
		got = append(got, string(ev)) // копия: срез валиден только внутри emit
	})
	if err != nil {
		t.Fatalf("scanEvents: %v", err)
	}
	if n != uint64(len(data)) {
		t.Fatalf("scanEvents прочитал %d байт, ожидалось %d", n, len(data))
	}
	return got
}

// oneByteReader выдаёт по байту за Read — стресс коротких дочиток.
type oneByteReader struct{ r io.Reader }

func (o oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return o.r.Read(p)
}

func TestScanEventsMatchesSplitEvents(t *testing.T) {
	ev := func(mask, body string) string { return mask + body + "\n" }
	inputs := map[string][]byte{
		"пустой":            {},
		"мусор без маски":   []byte("это не событие\nи это тоже\n"),
		"одно событие":      []byte(ev("00:01.000001-5,CALL,1,", "Usr=a")),
		"без хвостового \\n": []byte("00:01.000001-5,CALL,1,Usr=a\nстрока продолжения"),
		"BOM":               append([]byte{0xEF, 0xBB, 0xBF}, []byte(ev("00:02.000002-7,EXCP,0,", "x=1")+ev("00:03.000003-9,CALL,2,", "y=2"))...),
		"мусор до первой маски": []byte("prelude\n" + ev("10:00.000000-1,A,1,", "k=v") + ev("10:00.000001-2,B,2,", "k=w")),
		"многострочное событие": []byte(ev("11:11.111111-3,SDBL,1,", "Sql='select\n * from\n t'") + ev("11:11.111112-4,CALL,1,", "z=0")),
		"пустые строки":         []byte("00:01.000001-5,CALL,1,a=b\n\n\n00:01.000002-6,CALL,1,c=d\n"),
		"CRLF":                  []byte("00:01.000001-5,CALL,1,a=b\r\n00:01.000002-6,CALL,1,c=d\r\n"),
	}
	// Гигантское событие: тело больше чанка — буфер обязан вырасти
	var big bytes.Buffer
	big.WriteString("12:00.000000-100,TLOCK,1,Locks='")
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&big, "строка блокировки номер %d\n", i)
	}
	big.WriteString("'\n12:00.000001-1,CALL,1,ok=1\n")
	inputs["событие больше чанка"] = big.Bytes()

	// Плотный поток коротких событий — маски на всех переездах окна
	var many bytes.Buffer
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&many, "0%d:0%d.%06d-%d,CALL,%d,Usr=user%d,Mem=%d\n", i%10, i%6, i, i*7, i%4, i, i*100)
	}
	inputs["500 коротких событий"] = many.Bytes()

	chunks := []int{64, 128, 1024, 64 << 10}
	guards := []int{48, 256}
	for name, data := range inputs {
		want := collectSplit(data)
		for _, ch := range chunks {
			for _, g := range guards {
				got := collectScan(t, data, ch, g)
				if len(got) != len(want) {
					t.Errorf("%s (chunk=%d guard=%d): %d событий, ожидалось %d", name, ch, g, len(got), len(want))
					continue
				}
				for i := range want {
					if got[i] != want[i] {
						t.Errorf("%s (chunk=%d guard=%d): событие %d = %q, ожидалось %q", name, ch, g, i, got[i], want[i])
						break
					}
				}
			}
		}
	}
}

func TestScanEventsOneByteReads(t *testing.T) {
	data := []byte("00:01.000001-5,CALL,1,a=b\nпродолжение\n00:01.000002-6,EXCP,0,c=d\n")
	want := collectSplit(data)
	var got []string
	_, n, err := scanEvents(oneByteReader{bytes.NewReader(data)}, nil, 64, 48, func(ev []byte) {
		got = append(got, string(ev))
	})
	if err != nil {
		t.Fatalf("scanEvents: %v", err)
	}
	if n != uint64(len(data)) {
		t.Fatalf("прочитано %d байт, ожидалось %d", n, len(data))
	}
	if len(got) != len(want) {
		t.Fatalf("%d событий, ожидалось %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("событие %d = %q, ожидалось %q", i, got[i], want[i])
		}
	}
}

// errReader возвращает ошибку после первых байт — ошибка чтения должна дойти
// до вызывающего (счётчик failed_files).
type errReader struct {
	data []byte
	done bool
}

func (e *errReader) Read(p []byte) (int, error) {
	if e.done {
		return 0, errors.New("диск отвалился")
	}
	n := copy(p, e.data)
	e.done = true
	return n, nil
}

func TestScanEventsReadError(t *testing.T) {
	r := &errReader{data: []byte("00:01.000001-5,CALL,1,a=b\n")}
	_, _, err := scanEvents(r, nil, 64, 48, func([]byte) {})
	if err == nil {
		t.Fatal("ошибка чтения потеряна")
	}
}

func TestScanEventsReusesBuffer(t *testing.T) {
	data := []byte("00:01.000001-5,CALL,1,a=b\n00:01.000002-6,CALL,1,c=d\n")
	buf := make([]byte, 0, ReadChunk+GuardZone)
	buf2, _, err := ScanEvents(bytes.NewReader(data), buf, func([]byte) {})
	if err != nil {
		t.Fatalf("ScanEvents: %v", err)
	}
	if &buf[:1][0] != &buf2[:1][0] {
		t.Error("буфер достаточной ёмкости не переиспользован")
	}
}
