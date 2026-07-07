//! Нормализация технологического журнала 1С в NDJSON.
//!
//! Порт семантики `cpp_parse/count_contexts.cpp` байт-в-байт по спецификации
//! docs/format-spec.md v1.0 (ревизия 3). Любое отклонение от спеки — баг:
//! golden-суита (tests/golden/run_golden.ps1) сравнивает вывод побайтно.
//! Эталон паритета — Go-агент (agents/go/internal/parser/parser.go).

/// Файлы короче пропускаются целиком (format-spec §6).
pub const MIN_FILE_SIZE: u64 = 100;

/// Поиск байта в срезе (аналог bytes.IndexByte).
#[inline]
fn find(hay: &[u8], needle: u8) -> Option<usize> {
    hay.iter().position(|&c| c == needle)
}

/// Разбирает имя файла `YYMMDDHH.log` в префикс `20YY-MM-DDTHH:`.
/// Первые 8 символов обязаны быть цифрами, суффикс и диапазоны не проверяются
/// (format-spec §3, поле timestamp). Иначе — пустая строка (timestamp
/// деградирует до MM:SS.ssssss, файл считается аномалией).
pub fn date_from_filename(name: &str) -> String {
    let b = name.as_bytes();
    if b.len() < 8 || !b[..8].iter().all(u8::is_ascii_digit) {
        return String::new();
    }
    format!(
        "20{}-{}-{}T{}:",
        &name[0..2],
        &name[2..4],
        &name[4..6],
        &name[6..8]
    )
}

/// Маска начала события: `^\d{2}:\d{2}\.\d{6}-\d+,` (format-spec §2.1).
/// `b` — срез от начала физической строки до конца данных (маска может
/// «смотреть» за пределы строки, но `\n` не пройдёт проверку «цифра или запятая»).
pub fn is_event_start(b: &[u8]) -> bool {
    if b.len() < 15 {
        return false;
    }
    if !(b[0].is_ascii_digit()
        && b[1].is_ascii_digit()
        && b[2] == b':'
        && b[3].is_ascii_digit()
        && b[4].is_ascii_digit()
        && b[5] == b'.'
        && b[6].is_ascii_digit()
        && b[7].is_ascii_digit()
        && b[8].is_ascii_digit()
        && b[9].is_ascii_digit()
        && b[10].is_ascii_digit()
        && b[11].is_ascii_digit()
        && b[12] == b'-')
    {
        return false;
    }
    let mut has_digits = false;
    for &c in &b[13..] {
        match c {
            b'0'..=b'9' => has_digits = true,
            b',' => return has_digits,
            _ => return false,
        }
    }
    false
}

/// Строгая грамматика JSON-числа RFC 8259, длина ≤ 32 (format-spec §4.2, KI-2):
/// `-?(0|[1-9]\d*)(\.\d+)?([eE][+-]?\d+)?`
pub fn is_number_token(v: &[u8]) -> bool {
    if v.is_empty() || v.len() > 32 {
        return false;
    }
    let mut i = 0;
    if v[i] == b'-' {
        i += 1;
        if i == v.len() {
            return false;
        }
    }
    // Целая часть: 0 или [1-9][0-9]*
    match v[i] {
        b'0' => i += 1,
        b'1'..=b'9' => {
            while i < v.len() && v[i].is_ascii_digit() {
                i += 1;
            }
        }
        _ => return false,
    }
    // Дробная часть
    if i < v.len() && v[i] == b'.' {
        i += 1;
        if i == v.len() || !v[i].is_ascii_digit() {
            return false;
        }
        while i < v.len() && v[i].is_ascii_digit() {
            i += 1;
        }
    }
    // Экспонента
    if i < v.len() && (v[i] == b'e' || v[i] == b'E') {
        i += 1;
        if i < v.len() && (v[i] == b'+' || v[i] == b'-') {
            i += 1;
        }
        if i == v.len() || !v[i].is_ascii_digit() {
            return false;
        }
        while i < v.len() && v[i].is_ascii_digit() {
            i += 1;
        }
    }
    i == v.len()
}

/// Поля, которые никогда не типизируются числом (format-spec §4.2).
/// К `level` список НЕ применяется (§2.2).
fn is_always_string_field(name: &[u8]) -> bool {
    name == b"SearchString" || name == b"Guid" || name == b"UUID"
}

const HEX_DIGITS: &[u8; 16] = b"0123456789abcdef";

/// Дописывает `s` в `dst` с JSON-экранированием (format-spec §4.4):
/// `"`, `\`, \b \f \n \r \t, прочие < 0x20 → `\u00xx` (hex в нижнем регистре).
/// Байты ≥ 0x20 копируются как есть, UTF-8 не валидируется (KI-3).
pub fn append_escaped(dst: &mut Vec<u8>, s: &[u8]) {
    let mut start = 0;
    for (i, &c) in s.iter().enumerate() {
        if c >= 0x20 && c != b'"' && c != b'\\' {
            continue;
        }
        if i > start {
            dst.extend_from_slice(&s[start..i]);
        }
        match c {
            b'"' => dst.extend_from_slice(b"\\\""),
            b'\\' => dst.extend_from_slice(b"\\\\"),
            0x08 => dst.extend_from_slice(b"\\b"),
            0x0C => dst.extend_from_slice(b"\\f"),
            b'\n' => dst.extend_from_slice(b"\\n"),
            b'\r' => dst.extend_from_slice(b"\\r"),
            b'\t' => dst.extend_from_slice(b"\\t"),
            _ => dst.extend_from_slice(&[
                b'\\',
                b'u',
                b'0',
                b'0',
                HEX_DIGITS[(c >> 4) as usize],
                HEX_DIGITS[(c & 0x0f) as usize],
            ]),
        }
        start = i + 1;
    }
    if start < s.len() {
        dst.extend_from_slice(&s[start..]);
    }
}

/// Режет содержимое файла на события по маске начала строки (format-spec §2.1)
/// и вызывает `emit` для каждого. BOM в начале файла пропускается (KI-6).
/// Контент до первой строки-маски отбрасывается. Чётность кавычек НЕ
/// проверяется — KI-1 воспроизводится сознательно (golden-кейс
/// mask_inside_quotes остаётся XFAIL до починки в core).
///
/// В продакшн-пути заменён потоковым `scanner::scan_events` (файл целиком в
/// RAM не читается); остаётся эталонным оракулом для тестов сканера.
#[cfg(test)]
pub fn split_events(mut data: &[u8], mut emit: impl FnMut(&[u8])) {
    if data.len() >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
        data = &data[3..];
    }
    let n = data.len();
    let mut ptr = 0usize;
    let mut event_start = 0usize;
    let mut in_event = is_event_start(data);
    while ptr < n {
        match find(&data[ptr..], b'\n') {
            None => break,
            Some(idx) => {
                ptr += idx + 1;
                if ptr < n && is_event_start(&data[ptr..]) {
                    if in_event {
                        emit(&data[event_start..ptr]);
                    }
                    in_event = true;
                    event_start = ptr;
                }
            }
        }
    }
    if in_event && n > event_start {
        emit(&data[event_start..n]);
    }
}

/// Потребитель разобранного события. Автомат разбора один (`parse_event`),
/// эмиттеров два: [`JsonEmitter`] собирает NDJSON-строку (format-spec §3–4),
/// `chsink::RowEmitter` — RowBinary-строку для ClickHouse. Все данные приходят
/// СЫРЫМИ байтами источника: экранирование/типизация — забота эмиттера.
pub trait EventEmitter {
    /// Заголовок события: `time_part` — `ММ:СС.мммммм` (12 байт по маске §2.1),
    /// `duration` — цифры без ведущих нулей (KI-2), `level` — сырой токен.
    fn header(&mut self, time_part: &[u8], duration: &[u8], event: &[u8], level: &[u8]);
    /// Имя очередного свойства (всё до `=`).
    fn prop_name(&mut self, name: &[u8]);
    /// Открытие значения в кавычках (§4.1); пустое значение `Имя=` в конце
    /// события приходит как `quoted_begin` + `quoted_end`.
    fn quoted_begin(&mut self);
    /// Фрагмент значения в кавычках — сырые байты (включая внутренние \r\n).
    fn quoted_frag(&mut self, frag: &[u8]);
    /// Кавычка-данные внутри значения: `''` → `'`, `""` → `"`,
    /// а также KI-10-одиночная `'`, посчитанная данными.
    fn quoted_quote(&mut self, quote: u8);
    /// Закрытие значения в кавычках (в т.ч. незакрытого — по концу события).
    fn quoted_end(&mut self);
    /// Значение без кавычек — сырой токен до `,`/конца события (типизация §4.2
    /// — забота эмиттера, поэтому передаётся и имя).
    fn unquoted(&mut self, name: &[u8], val: &[u8]);
    /// Конец события (заголовок и все свойства выданы).
    fn finish(&mut self);
}

/// Разбирает одно событие и скармливает его частями эмиттеру `em`.
/// Возвращает `false`, если событие отбрасывается (нет второй запятой
/// в заголовке и т.п. — parse_skip, format-spec §6); эмиттер в этом случае
/// не вызывается вовсе.
pub fn parse_event<E: EventEmitter>(ev: &[u8], em: &mut E) -> bool {
    // Хвостовые \r\n события обрезаются (внутренние сохраняются), §2.1
    let mut end = ev.len();
    while end > 0 && (ev[end - 1] == b'\n' || ev[end - 1] == b'\r') {
        end -= 1;
    }
    let ev = &ev[..end];
    if ev.is_empty() {
        return false;
    }

    // Заголовок: ММ:СС.мммммм-Длительность,Событие,Уровень[,...] (§2.2)
    let comma = match find(ev, b',') {
        Some(i) => i,
        None => return false,
    };
    let dash = match find(&ev[..comma], b'-') {
        Some(i) => i,
        None => return false,
    };
    let time_part = &ev[..dash];
    let mut duration = &ev[dash + 1..comma];
    // Канонизация duration: сырые байты источника минус ведущие нули,
    // "000" → "0" (KI-2). Никакого int/float round-trip.
    while duration.len() > 1 && duration[0] == b'0' {
        duration = &duration[1..];
    }

    let mut p = comma + 1;
    let rel = match find(&ev[p..], b',') {
        Some(i) => i,
        // Нет второй запятой после имени события → parse_skip (§6)
        None => return false,
    };
    let event_name = &ev[p..p + rel];
    p += rel + 1;

    // Уровень — до следующей запятой; если её нет, level съедает весь остаток
    // события и свойства не разбираются (golden-кейс short_header)
    let level: &[u8];
    if let Some(rel2) = find(&ev[p..], b',') {
        level = &ev[p..p + rel2];
        p += rel2 + 1;
    } else {
        level = &ev[p..];
        p = ev.len();
    }

    em.header(time_part, duration, event_name, level);
    parse_props(ev, p, em);
    em.finish();
    true
}

/// Эмиттер NDJSON-записи по format-spec (байт-в-байт с эталоном — golden-суита
/// сверяет побайтно; любое отклонение — баг).
pub struct JsonEmitter<'a> {
    dst: &'a mut Vec<u8>,
    date_prefix: &'a str,
    filename_esc: &'a [u8],
    file_path_esc: &'a [u8],
}

impl EventEmitter for JsonEmitter<'_> {
    #[inline]
    fn header(&mut self, time_part: &[u8], duration: &[u8], event: &[u8], level: &[u8]) {
        let dst = &mut *self.dst;
        dst.extend_from_slice(b"{\"timestamp\":\"");
        dst.extend_from_slice(self.date_prefix.as_bytes());
        dst.extend_from_slice(time_part); // маска гарантирует только цифры/':'/'.'
        dst.extend_from_slice(b"\",\"duration\":");
        dst.extend_from_slice(duration);
        dst.extend_from_slice(b",\"event\":\"");
        append_escaped(dst, event);
        dst.extend_from_slice(b"\",\"level\":");
        if is_number_token(level) {
            dst.extend_from_slice(level);
        } else {
            dst.push(b'"');
            append_escaped(dst, level);
            dst.push(b'"');
        }
        dst.extend_from_slice(b",\"filename\":\"");
        dst.extend_from_slice(self.filename_esc);
        dst.extend_from_slice(b"\",\"file_path\":\"");
        dst.extend_from_slice(self.file_path_esc);
        dst.push(b'"');
    }

    #[inline]
    fn prop_name(&mut self, name: &[u8]) {
        self.dst.extend_from_slice(b",\"");
        append_escaped(self.dst, name);
        self.dst.extend_from_slice(b"\":");
    }

    #[inline]
    fn quoted_begin(&mut self) {
        self.dst.push(b'"');
    }

    #[inline]
    fn quoted_frag(&mut self, frag: &[u8]) {
        append_escaped(self.dst, frag);
    }

    #[inline]
    fn quoted_quote(&mut self, quote: u8) {
        if quote == b'"' {
            self.dst.extend_from_slice(b"\\\"");
        } else {
            self.dst.push(quote);
        }
    }

    #[inline]
    fn quoted_end(&mut self) {
        self.dst.push(b'"');
    }

    #[inline]
    fn unquoted(&mut self, name: &[u8], val: &[u8]) {
        // Число по строгой грамматике, кроме always-string полей. Числа
        // эмитятся СЫРЫМИ байтами источника — без round-trip.
        if !is_always_string_field(name) && is_number_token(val) {
            self.dst.extend_from_slice(val);
        } else {
            self.dst.push(b'"');
            append_escaped(self.dst, val);
            self.dst.push(b'"');
        }
    }

    #[inline]
    fn finish(&mut self) {
        self.dst.push(b'}');
        self.dst.push(b'\n');
    }
}

/// Разбирает одно событие и дописывает в `dst` готовую JSON-строку
/// с завершающим `\n` (обёртка `parse_event` + [`JsonEmitter`]). Возвращает
/// `false`, если событие отбрасывается (parse_skip, format-spec §6);
/// `dst` при этом не меняется.
///
/// `date_prefix` — `20YY-MM-DDTHH:` или пустая строка; `filename_esc` /
/// `file_path_esc` — уже JSON-экранированные значения (общие на файл).
pub fn append_event(
    dst: &mut Vec<u8>,
    ev: &[u8],
    date_prefix: &str,
    filename_esc: &[u8],
    file_path_esc: &[u8],
) -> bool {
    let mut em = JsonEmitter {
        dst,
        date_prefix,
        filename_esc,
        file_path_esc,
    };
    parse_event(ev, &mut em)
}

/// Автомат свойств: имя до `=`, значение по правилам кавычек §4.1 либо без
/// кавычек до `,` (§4.2). Хвост без `=` молча отбрасывается. Единственный
/// экземпляр логики кавычек на оба синка (NDJSON и ClickHouse) — семантика
/// KI-10 и несимметричного закрытия живёт только здесь.
fn parse_props<E: EventEmitter>(ev: &[u8], mut p: usize, em: &mut E) {
    let end = ev.len();
    while p < end {
        let eq_pos = match find(&ev[p..end], b'=') {
            Some(i) => p + i,
            None => break,
        };
        let name = &ev[p..eq_pos];
        em.prop_name(name);

        p = eq_pos + 1;
        if p >= end {
            // `Имя=` последним байтом события → пустая строка
            em.quoted_begin();
            em.quoted_end();
            break;
        }

        match ev[p] {
            b'\'' => {
                // Одинарные кавычки: '' — экранирование; одиночная ' закрывает
                // значение только перед ',' или концом события (KI-10)
                em.quoted_begin();
                p += 1;
                let mut val_start = p;
                let mut closed = false;
                while p < end {
                    match find(&ev[p..end], b'\'') {
                        None => {
                            em.quoted_frag(&ev[val_start..end]);
                            em.quoted_end();
                            p = end;
                            closed = true;
                            break;
                        }
                        Some(idx) => {
                            p += idx;
                            if p + 1 < end && ev[p + 1] == b'\'' {
                                // Экранирование '' → одна кавычка в данных
                                em.quoted_frag(&ev[val_start..p]);
                                em.quoted_quote(b'\'');
                                p += 2;
                                val_start = p;
                            } else if p + 1 == end || ev[p + 1] == b',' {
                                // Закрывающая кавычка
                                em.quoted_frag(&ev[val_start..p]);
                                em.quoted_end();
                                p += 1;
                                closed = true;
                                break;
                            } else {
                                // Битый формат: одиночная ' внутри — считаем данными
                                em.quoted_frag(&ev[val_start..p]);
                                em.quoted_quote(b'\'');
                                p += 1;
                                val_start = p;
                            }
                        }
                    }
                }
                if !closed {
                    // Событие оборвалось ровно на экранирующей паре (§4.1):
                    // накопленное эмитим, значение закрываем
                    em.quoted_frag(&ev[val_start..p]);
                    em.quoted_end();
                }
            }
            b'"' => {
                // Двойные кавычки: "" — экранирование; первая одиночная "
                // закрывает безусловно (§4.1, несимметрично с одинарными!)
                em.quoted_begin();
                p += 1;
                let mut val_start = p;
                let mut closed = false;
                while p < end {
                    match find(&ev[p..end], b'"') {
                        None => {
                            em.quoted_frag(&ev[val_start..end]);
                            em.quoted_end();
                            p = end;
                            closed = true;
                            break;
                        }
                        Some(idx) => {
                            p += idx;
                            if p + 1 < end && ev[p + 1] == b'"' {
                                em.quoted_frag(&ev[val_start..p]);
                                em.quoted_quote(b'"');
                                p += 2;
                                val_start = p;
                                continue;
                            }
                            em.quoted_frag(&ev[val_start..p]);
                            em.quoted_end();
                            p += 1;
                            closed = true;
                            break;
                        }
                    }
                }
                if !closed {
                    em.quoted_frag(&ev[val_start..p]);
                    em.quoted_end();
                }
            }
            _ => {
                // Без кавычек: сырой токен до ',' или конца события
                let sep_pos = match find(&ev[p..end], b',') {
                    Some(i) => p + i,
                    None => end,
                };
                em.unquoted(name, &ev[p..sep_pos]);
                p = sep_pos;
            }
        }

        if p < end && ev[p] == b',' {
            p += 1;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn date_prefix() {
        assert_eq!(date_from_filename("25113021.log"), "2025-11-30T21:");
        assert_eq!(date_from_filename("notadate.log"), "");
        assert_eq!(date_from_filename("2511302.log"), ""); // 8-й символ '.' — не цифра
    }

    #[test]
    fn date_prefix_short_and_nondigit() {
        assert_eq!(date_from_filename("1234567"), "");
        assert_eq!(date_from_filename("1234567a.log"), "");
    }

    #[test]
    fn number_token() {
        for ok in ["0", "-1", "12.5", "1e10", "1.5E-3", "17500000000"] {
            assert!(is_number_token(ok.as_bytes()), "{ok}");
        }
        for bad in [
            "", "007", "8.3.22.1704", "1-2", ".5", "0.", "1.", "+1", "1e", "-",
            "0x10", " 1", "123456789012345678901234567890123",
        ] {
            assert!(!is_number_token(bad.as_bytes()), "{bad}");
        }
    }

    #[test]
    fn event_start_mask() {
        assert!(is_event_start(b"10:00.000000-5,CALL,0"));
        assert!(is_event_start("10:00.000000-5,мусор".as_bytes())); // §2.1: сплит не смотрит дальше запятой
        assert!(!is_event_start(b"10:00.000000-,X"));
        assert!(!is_event_start(b"1:00.000000-5,X"));
        assert!(!is_event_start(b"10:00.00000-5,X"));
    }

    #[test]
    fn escaping() {
        let mut dst = Vec::new();
        append_escaped(&mut dst, b"a\"b\\c\nd\x01e");
        assert_eq!(dst, b"a\\\"b\\\\c\\nd\\u0001e");
    }

    fn parse_one(ev: &str) -> Option<String> {
        let mut dst = Vec::new();
        if append_event(&mut dst, ev.as_bytes(), "2025-11-30T10:", b"f.log", b"in\\\\p\\\\f.log") {
            Some(String::from_utf8(dst).unwrap())
        } else {
            None
        }
    }

    #[test]
    fn short_header_level_eats_rest() {
        // §2.2: нет запятой после уровня → level поглощает остаток
        let out = parse_one("00:01.000001-2,EXCP,Pad=xxx").unwrap();
        assert!(out.contains("\"level\":\"Pad=xxx\""), "{out}");
        assert!(!out.contains("\"Pad\":"), "{out}");
    }

    #[test]
    fn no_second_comma_is_skip() {
        assert!(parse_one("00:01.000001-2,EXCP").is_none());
    }

    #[test]
    fn leading_zero_duration() {
        let out = parse_one("00:01.000001-007,CALL,0").unwrap();
        assert!(out.contains("\"duration\":7,"), "{out}");
        let out = parse_one("00:01.000001-000,CALL,0").unwrap();
        assert!(out.contains("\"duration\":0,"), "{out}");
    }

    #[test]
    fn version_token_stays_string() {
        let out = parse_one("00:01.000001-2,CALL,0,AppVer=8.3.22.1704,N=42").unwrap();
        assert!(out.contains("\"AppVer\":\"8.3.22.1704\""), "{out}");
        assert!(out.contains("\"N\":42"), "{out}");
    }

    #[test]
    fn quote_doubling() {
        let out = parse_one("00:01.000001-2,CALL,0,A='x''y',B=\"p\"\"q\"").unwrap();
        assert!(out.contains("\"A\":\"x'y\""), "{out}");
        assert!(out.contains("\"B\":\"p\\\"q\""), "{out}");
    }

    #[test]
    fn split_skips_bom_and_preamble() {
        let data = b"\xEF\xBB\xBF00:01.000001-2,CALL,0\n00:02.000001-3,EXCP,1\n";
        let mut got = Vec::new();
        split_events(data, |ev| got.push(ev.to_vec()));
        assert_eq!(got.len(), 2);
        assert!(got[0].starts_with(b"00:01"));
        assert!(got[1].starts_with(b"00:02"));
    }
}
