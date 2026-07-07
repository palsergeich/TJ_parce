//! Чанковое чтение файла с нарезкой на события — потоковый аналог
//! `parser::split_events` (семантика байт-в-байт, оракул в тестах ниже).
//!
//! Зеркало core/src/pipeline.cpp::scan_file_windowed: файл читается кусками
//! фиксированного размера, внутри буфера ищутся строки-маски; хвост от начала
//! текущего события переносится в начало буфера (событие закрывается только
//! СЛЕДУЮЩЕЙ строкой-маской или EOF). Кандидат маски у края буфера решается,
//! только когда после него доступно ≥ GUARD байт (или EOF) — как гвардейская
//! зона core (64 КБ: маске нужно ~15 байт + цифры длительности).
//!
//! Резидентность на файл: ~CHUNK + перенос (обычно << CHUNK; гигантское
//! событие растит буфер — прогресс гарантирован, как в core).

use std::io::{self, Read};

use crate::parser::is_event_start;

/// Размер куска чтения. 32 МБ — в коридоре 8–64 МБ требований и заметно
/// больше GUARD; крупный кусок снижает число стыков и syscall'ов.
pub const CHUNK_BYTES: usize = 32 << 20;
/// Гвардейская зона у конца незавершённого буфера (паритет с core kMapGuard).
const GUARD_BYTES: usize = 64 << 10;

/// Поиск '\n' по 8 байт за шаг (SWAR, только std): ~память-bound вместо
/// побайтового сравнения — это самый горячий цикл нормализатора.
#[inline]
fn find_nl(hay: &[u8]) -> Option<usize> {
    const LO: u64 = 0x0101_0101_0101_0101;
    const HI: u64 = 0x8080_8080_8080_8080;
    const NL: u64 = 0x0A0A_0A0A_0A0A_0A0A;
    let mut i = 0usize;
    let words = hay.len() - hay.len() % 8;
    while i < words {
        // 8 байт little-endian; выравнивание некритично на x86
        let v = u64::from_le_bytes(hay[i..i + 8].try_into().unwrap()) ^ NL;
        let m = v.wrapping_sub(LO) & !v & HI;
        if m != 0 {
            return Some(i + (m.trailing_zeros() >> 3) as usize);
        }
        i += 8;
    }
    hay[words..].iter().position(|&c| c == b'\n').map(|j| words + j)
}

/// Читает `r` кусками `CHUNK_BYTES` и вызывает `emit` для каждого события
/// (семантика `parser::split_events`, включая пропуск BOM — KI-6).
/// `buf` — переиспользуемый буфер вызывающего (ёмкость сохраняется между
/// файлами). Возвращает число прочитанных байт; `Err` — ошибка чтения посреди
/// файла (уже выданные события остаются выданными, хвост не эмитится —
/// паритет с core, где ошибка отображения прерывает файл).
pub fn scan_events<R: Read>(
    r: &mut R,
    buf: &mut Vec<u8>,
    emit: impl FnMut(&[u8]),
) -> io::Result<u64> {
    scan_events_impl(r, buf, CHUNK_BYTES, GUARD_BYTES, emit)
}

fn scan_events_impl<R: Read>(
    r: &mut R,
    buf: &mut Vec<u8>,
    chunk: usize,
    guard: usize,
    mut emit: impl FnMut(&[u8]),
) -> io::Result<u64> {
    // Длина буфера управляется вручную (data_len): buf.len() только растёт,
    // байты за data_len — мусор прошлых чтений. Это убирает повторное
    // зануление resize'ом целого куска на каждой итерации (лишний проход
    // по памяти на гигабайтных файлах).
    let mut data_len = 0usize;
    let mut total: u64 = 0;
    let mut eof = false;
    let mut first_fill = true;
    // Статус позиции 0 (после BOM) решается при первом же скане с достаточным
    // контекстом — та же гвардейская дисциплина, что у кандидатов после '\n'.
    let mut decided = false;
    let mut in_event = false;
    let mut scan = 0usize; // позиция сканирования в buf
    let mut ev_start = 0usize; // начало текущего события в buf (значимо при in_event)

    loop {
        // Дочитываем до полного куска: read может вернуть меньше запрошенного.
        if buf.len() < data_len + chunk {
            buf.resize(data_len + chunk, 0);
        }
        let mut filled = 0usize;
        while filled < chunk {
            match r.read(&mut buf[data_len + filled..data_len + chunk]) {
                Ok(0) => {
                    eof = true;
                    break;
                }
                Ok(n) => filled += n,
                Err(e) if e.kind() == io::ErrorKind::Interrupted => {}
                Err(e) => return Err(e),
            }
        }
        data_len += filled;
        total += filled as u64;

        if first_fill {
            first_fill = false;
            // BOM в начале файла пропускается (KI-6)
            if data_len >= 3 && buf[..3] == [0xEF, 0xBB, 0xBF] {
                scan = 3;
                ev_start = 3;
            }
        }

        let end = data_len;
        // Не EOF ⇒ filled == chunk ⇒ end ≥ chunk; при chunk > guard
        // safe_end > 0 и кандидаты решаются с ≥ guard байт контекста.
        let safe_end = if eof { end } else { end.saturating_sub(guard) };

        if !decided && (eof || scan < safe_end) {
            in_event = is_event_start(&buf[scan..end]);
            decided = true;
        }

        while scan < safe_end {
            let nl = match find_nl(&buf[scan..safe_end]) {
                None => {
                    scan = safe_end;
                    break;
                }
                Some(i) => scan + i,
            };
            let next_line = nl + 1;
            scan = next_line;
            // Кандидат решается сразу: после next_line ≥ guard байт (или EOF).
            if next_line < end && is_event_start(&buf[next_line..end]) {
                if in_event {
                    emit(&buf[ev_start..next_line]);
                }
                in_event = true;
                ev_start = next_line;
            }
        }

        if eof {
            break;
        }

        // Перенос хвоста в начало буфера: от начала текущего события, а без
        // события — от позиции сканирования (преамбула не копится в RAM).
        let keep_from = if in_event { ev_start } else { scan };
        if keep_from > 0 {
            buf.copy_within(keep_from..data_len, 0);
            data_len -= keep_from;
            scan -= keep_from;
            ev_start = if in_event { ev_start - keep_from } else { scan };
        }
    }

    // Последнее событие закрывается EOF
    if in_event && data_len > ev_start {
        emit(&buf[ev_start..data_len]);
    }
    Ok(total)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::parser::split_events;
    use std::io::Cursor;

    #[test]
    fn find_nl_all_offsets() {
        let mut v = vec![b'x'; 41];
        assert_eq!(find_nl(&v), None);
        for i in 0..v.len() {
            let mut w = v.clone();
            w[i] = b'\n';
            assert_eq!(find_nl(&w), Some(i), "pos {i}");
            w[40] = b'\n'; // второй '\n' дальше — возвращается первый
            assert_eq!(find_nl(&w), Some(i.min(40)), "pos {i}+tail");
        }
        v.clear();
        assert_eq!(find_nl(&v), None);
    }

    /// Прогоняет данные через чанковый сканер и через оракул split_events,
    /// сверяет списки событий байт-в-байт.
    fn check(data: &[u8], chunk: usize, guard: usize) {
        let mut expected: Vec<Vec<u8>> = Vec::new();
        split_events(data, |ev| expected.push(ev.to_vec()));

        let mut got: Vec<Vec<u8>> = Vec::new();
        // Буфер с мусором ненулевой длины: сканер обязан игнорировать байты
        // за data_len (проверка ручного управления длиной)
        let mut buf = vec![0xAAu8; 7];
        let total = scan_events_impl(&mut Cursor::new(data), &mut buf, chunk, guard, |ev| {
            got.push(ev.to_vec())
        })
        .unwrap();
        assert_eq!(total, data.len() as u64, "chunk={chunk} guard={guard}");
        assert_eq!(got, expected, "chunk={chunk} guard={guard}");
    }

    /// Все комбинации маленьких chunk/guard — переносы через край на каждом байте.
    fn check_all(data: &[u8]) {
        for chunk in [32usize, 64, 128, 1024, 1 << 20] {
            for guard in [24usize, 31] {
                assert!(chunk > guard);
                check(data, chunk, guard);
            }
        }
    }

    #[test]
    fn matches_oracle_simple() {
        check_all(b"00:01.000001-2,CALL,0,A=1\n00:02.000002-3,EXCP,1,B=2\n");
    }

    #[test]
    fn matches_oracle_multiline_and_preamble() {
        let data = b"garbage preamble\nmore garbage\n00:01.000001-2,CALL,0,Ctx='line1\nline2\nline3'\n00:02.000002-3,EXCP,1\ntail line no mask\n00:03.000003-4,CALL,0,X=9\n";
        check_all(data);
    }

    #[test]
    fn matches_oracle_bom_and_crlf() {
        let data = b"\xEF\xBB\xBF00:01.000001-2,CALL,0,A=1\r\n00:02.000002-3,EXCP,1\r\n";
        check_all(data);
    }

    #[test]
    fn matches_oracle_no_trailing_newline() {
        check_all(b"00:01.000001-2,CALL,0,A=1\n00:02.000002-3,EXCP,1,B=2");
    }

    #[test]
    fn matches_oracle_empty_and_tiny() {
        check_all(b"");
        check_all(b"\n");
        check_all(b"\xEF\xBB\xBF");
        check_all(b"no events at all\njust text\n");
        check_all(b"00:01.000001-2,CALL,0");
    }

    #[test]
    fn matches_oracle_event_bigger_than_chunk() {
        // Событие много больше куска: буфер обязан расти без потери байтов
        let mut data = Vec::new();
        data.extend_from_slice(b"00:01.000001-2,CALL,0,Big='");
        for i in 0..500 {
            data.extend_from_slice(format!("line {i} of a very long value\n").as_bytes());
        }
        data.extend_from_slice(b"end'\n00:02.000002-3,EXCP,1\n");
        check_all(&data);
    }

    #[test]
    fn matches_oracle_mask_at_every_offset() {
        // Маска события скользит по всем смещениям относительно края куска
        let mut data = Vec::new();
        for i in 0..200 {
            data.extend_from_slice(
                format!("00:01.{:06}-{},CALL,0,N={},Pad='xy z{}'\n", i, i + 1, i, "q".repeat(i % 17))
                    .as_bytes(),
            );
        }
        check_all(&data);
    }
}
