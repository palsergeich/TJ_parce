package follow

import (
	"bytes"
	"fmt"
	"testing"
)

type emitted struct {
	ev  string
	end int64
}

func feed(a *assembler, data string) []emitted {
	var out []emitted
	a.append([]byte(data), func(ev []byte, end int64) {
		out = append(out, emitted{string(ev), end})
	})
	return out
}

// Правило 1: следующая строка-маска закрывает событие.
func TestMaskClosesEvent(t *testing.T) {
	var a assembler
	got := feed(&a, "11:22.333333-100,EV,1,a=1\n11:22.333334-1,EV2,1,b=2\n")
	if len(got) != 1 {
		t.Fatalf("эмитов %d, ожидался 1: %+v", len(got), got)
	}
	if got[0].ev != "11:22.333333-100,EV,1,a=1\n" {
		t.Errorf("событие %q", got[0].ev)
	}
	if got[0].end != 26 {
		t.Errorf("end = %d, want 26", got[0].end)
	}
	// Второе событие ещё в pending (нет следующей маски/idle/стопа)
	if !a.inEvent || a.readOff() != int64(len("11:22.333333-100,EV,1,a=1\n11:22.333334-1,EV2,1,b=2\n")) {
		t.Errorf("состояние: inEvent=%v readOff=%d", a.inEvent, a.readOff())
	}
}

// Многострочное событие: строки-продолжения не закрывают его.
func TestMultilineEvent(t *testing.T) {
	var a assembler
	ev1 := "11:22.333333-100,EV,1,Txt='line1\r\nline2'\n"
	got := feed(&a, ev1+"11:22.333334-1,EV2,1,b=2\n")
	if len(got) != 1 || got[0].ev != ev1 {
		t.Fatalf("эмиты: %+v", got)
	}
}

// Незавершённая строка (без \n) не участвует в решениях и не эмитится.
func TestPartialLineNeverEmitted(t *testing.T) {
	var a assembler
	if got := feed(&a, "11:22.333333-100,EV,1,a=1\n11:22.33"); len(got) != 0 {
		t.Fatalf("эмит по незавершённой строке: %+v", got)
	}
	// Достройка маски и завершение строки закрывает первое событие
	got := feed(&a, "3334-1,EV2,1,b=2\n")
	if len(got) != 1 || got[0].ev != "11:22.333333-100,EV,1,a=1\n" {
		t.Fatalf("эмиты: %+v", got)
	}
	// Драйн на стопе отдаёт второе событие (оно \n-терминировано)
	var drained []emitted
	a.drain(func(ev []byte, end int64) { drained = append(drained, emitted{string(ev), end}) })
	if len(drained) != 1 || drained[0].ev != "11:22.333334-1,EV2,1,b=2\n" {
		t.Fatalf("drain: %+v", drained)
	}
}

// Побайтовая подача эквивалентна цельной (независимость от чанков чтения).
func TestByteAtATimeEquivalence(t *testing.T) {
	data := "\xEF\xBB\xBFgarbage\n11:22.333333-100,EV,1,Txt='a\nb'\n11:22.333334-1,EV2,1,b=2\n33:44.555555-6,EV3,0,c=3\n"
	var whole, byByte []emitted
	var a1, a2 assembler
	a1.append([]byte(data), func(ev []byte, end int64) { whole = append(whole, emitted{string(ev), end}) })
	for i := 0; i < len(data); i++ {
		a2.append([]byte{data[i]}, func(ev []byte, end int64) { byByte = append(byByte, emitted{string(ev), end}) })
	}
	if fmt.Sprint(whole) != fmt.Sprint(byByte) {
		t.Errorf("целиком: %+v\nпобайтово: %+v", whole, byByte)
	}
	if len(whole) != 2 {
		t.Errorf("эмитов %d, ожидалось 2: %+v", len(whole), whole)
	}
}

// BOM пропускается только на оффсете 0; мусор до первой маски отбрасывается,
// но офcеты (end) считаются по исходному файлу.
func TestBOMAndGarbagePrefix(t *testing.T) {
	var a assembler
	prefix := "\xEF\xBB\xBFмусор\n"
	ev1 := "11:22.333333-100,EV,1,a=1\n"
	got := feed(&a, prefix+ev1+"11:22.333334-1,EV2,1,b=2\n")
	if len(got) != 1 || got[0].ev != ev1 {
		t.Fatalf("эмиты: %+v", got)
	}
	if want := int64(len(prefix) + len(ev1)); got[0].end != want {
		t.Errorf("end = %d, want %d (учёт BOM и мусора)", got[0].end, want)
	}
}

// Резюме с чекпоинта: BOM не ищется, база оффсетов — точка резюме.
func TestResumeAtOffset(t *testing.T) {
	var a assembler
	a.resumeAt(1000)
	ev1 := "11:22.333333-100,EV,1,a=1\n"
	got := feed(&a, ev1+"11:22.333334-1,EV2,1,b=2\n")
	if len(got) != 1 || got[0].end != 1000+int64(len(ev1)) {
		t.Fatalf("эмиты: %+v", got)
	}
}

// Правило 2: idleEmit срабатывает только когда pending целиком \n-терминирован.
func TestIdleEmitOnlyWhenNewlineTerminated(t *testing.T) {
	var a assembler
	feed(&a, "11:22.333333-100,EV,1,a=1\n11:22.33") // хвост без \n
	if a.idleEmit(func([]byte, int64) { t.Fatal("эмит при незавершённой строке") }) {
		t.Fatal("idleEmit=true при незавершённой строке")
	}
	var got []emitted
	feed(&a, "3334-1,EV2,1,b=2\n") // первое событие закрылось маской
	if !a.idleEmit(func(ev []byte, end int64) { got = append(got, emitted{string(ev), end}) }) {
		t.Fatal("idleEmit=false на \\n-терминированном pending")
	}
	if len(got) != 1 || got[0].ev != "11:22.333334-1,EV2,1,b=2\n" {
		t.Fatalf("idle-эмит: %+v", got)
	}
	if a.inEvent || len(a.pending) != 0 {
		t.Errorf("после idle-эмита: inEvent=%v pending=%d", a.inEvent, len(a.pending))
	}
	// Строка-продолжение после idle-эмита — мусор до следующей маски
	if got := feed(&a, "поздний хвост\n"); len(got) != 0 {
		t.Fatalf("поздний хвост эмитирован: %+v", got)
	}
}

// Правило 3: drain отдаёт только \n-терминированную часть события.
func TestDrainKeepsIncompleteLine(t *testing.T) {
	var a assembler
	feed(&a, "11:22.333333-100,EV,1,a=1\nвторая строка\nобрыв без перевода")
	var got []emitted
	a.drain(func(ev []byte, end int64) { got = append(got, emitted{string(ev), end}) })
	if len(got) != 1 || got[0].ev != "11:22.333333-100,EV,1,a=1\nвторая строка\n" {
		t.Fatalf("drain: %+v", got)
	}
	if got[0].end != int64(len("11:22.333333-100,EV,1,a=1\nвторая строка\n")) {
		t.Errorf("end = %d", got[0].end)
	}
}

// Маска не должна решаться по префиксу: строка, похожая на маску, но без
// запятой после длительности — продолжение, а не новое событие.
func TestMaskLookalikeContinuation(t *testing.T) {
	var a assembler
	ev1 := "11:22.333333-100,EV,1,a=1\n"
	notMask := "11:22.333334-99\n" // нет запятой — не маска
	got := feed(&a, ev1+notMask+"11:22.333335-1,EV2,1,b=2\n")
	if len(got) != 1 || got[0].ev != ev1+notMask {
		t.Fatalf("эмиты: %+v", got)
	}
}

// Усечение: reset возвращает чистое состояние с проверкой BOM.
func TestResetAfterTruncate(t *testing.T) {
	var a assembler
	feed(&a, "11:22.333333-100,EV,1,a=1\nхвост")
	a.reset()
	if a.readOff() != 0 || a.inEvent || a.bomDone {
		t.Fatalf("после reset: %+v", a)
	}
	got := feed(&a, "\xEF\xBB\xBF11:22.333333-1,EV,1,a=1\n22:33.444444-2,EV2,1,b=2\n")
	if len(got) != 1 || !bytes.Equal([]byte(got[0].ev), []byte("11:22.333333-1,EV,1,a=1\n")) {
		t.Fatalf("после reset: %+v", got)
	}
}
