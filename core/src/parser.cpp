// parser.cpp — разрезание на события, автомат свойств, эмиссия JSON и RowBinary.
// Семантика: docs/format-spec.md v1.0 rev 3, побайтная совместимость с эталоном.
// Разбор события общий для обоих выходных форматов (parse_event_header +
// append_props_impl<Policy>): NDJSON и RowBinary не могут разойтись по
// множеству принятых событий или по правилам кавычек.
#include "parser.hpp"

namespace tj {
namespace parse {

namespace {

inline bool is_digit(char c) { return c >= '0' && c <= '9'; }

// Поля, которые никогда не типизируются числом (format-spec §4.2).
inline bool is_always_string_field(const char* name, std::size_t n) {
    switch (n) {
        case 4:  return std::memcmp(name, "Guid", 4) == 0 || std::memcmp(name, "UUID", 4) == 0;
        case 12: return std::memcmp(name, "SearchString", 12) == 0;
        default: return false;
    }
}

constexpr char kHexDigits[] = "0123456789abcdef";

} // namespace

bool is_event_start(const char* b, std::size_t n) {
    if (n < 15) return false;
    if (!(is_digit(b[0]) && is_digit(b[1]) && b[2] == ':' &&
          is_digit(b[3]) && is_digit(b[4]) && b[5] == '.' &&
          is_digit(b[6]) && is_digit(b[7]) && is_digit(b[8]) &&
          is_digit(b[9]) && is_digit(b[10]) && is_digit(b[11]) &&
          b[12] == '-')) {
        return false;
    }
    bool has_digits = false;
    for (std::size_t i = 13; i < n; ++i) {
        char c = b[i];
        if (c >= '0' && c <= '9') {
            has_digits = true;
        } else if (c == ',') {
            return has_digits;
        } else {
            return false;
        }
    }
    return false;
}

// -?(0|[1-9]\d*)(\.\d+)?([eE][+-]?\d+)?, длина ≤ 32 (KI-2: строгая грамматика).
bool is_number_token(const char* v, std::size_t n) {
    if (n == 0 || n > 32) return false;
    std::size_t i = 0;
    if (v[i] == '-') {
        ++i;
        if (i == n) return false;
    }
    // Целая часть: 0 или [1-9][0-9]*
    if (v[i] == '0') {
        ++i;
    } else if (v[i] >= '1' && v[i] <= '9') {
        while (i < n && is_digit(v[i])) ++i;
    } else {
        return false;
    }
    // Дробная часть
    if (i < n && v[i] == '.') {
        ++i;
        if (i == n || !is_digit(v[i])) return false;
        while (i < n && is_digit(v[i])) ++i;
    }
    // Экспонента
    if (i < n && (v[i] == 'e' || v[i] == 'E')) {
        ++i;
        if (i < n && (v[i] == '+' || v[i] == '-')) ++i;
        if (i == n || !is_digit(v[i])) return false;
        while (i < n && is_digit(v[i])) ++i;
    }
    return i == n;
}

void append_escaped(std::string& dst, const char* s, std::size_t n) {
    std::size_t start = 0;
    for (std::size_t i = 0; i < n; ++i) {
        unsigned char c = static_cast<unsigned char>(s[i]);
        if (c >= 0x20 && c != '"' && c != '\\') continue;
        if (i > start) dst.append(s + start, i - start);
        switch (c) {
            case '"':  dst.append("\\\"", 2); break;
            case '\\': dst.append("\\\\", 2); break;
            case '\b': dst.append("\\b", 2); break;
            case '\f': dst.append("\\f", 2); break;
            case '\n': dst.append("\\n", 2); break;
            case '\r': dst.append("\\r", 2); break;
            case '\t': dst.append("\\t", 2); break;
            default: {
                char buf[6] = {'\\', 'u', '0', '0',
                               kHexDigits[c >> 4], kHexDigits[c & 0x0f]};
                dst.append(buf, 6);
                break;
            }
        }
        start = i + 1;
    }
    if (start < n) dst.append(s + start, n - start);
}

namespace {

// --- Общий разбор заголовка события ------------------------------------------
// Заголовок: ММ:СС.мммммм-Длительность,Событие,Уровень[,...] (§2.2).
// Условия parse_skip (нет запятой / '-' / второй запятой, пустое событие после
// трима §6) — ЕДИНЫЕ для NDJSON и RowBinary.
struct EventHeader {
    const char* time_b; std::size_t time_n;
    const char* dur_b;  std::size_t dur_n;  // канонизировано: без ведущих нулей (KI-2)
    const char* name_b; std::size_t name_n;
    const char* lvl_b;  std::size_t lvl_n;
    std::size_t props_off; // смещение начала свойств (== n, если свойств нет)
    std::size_t n;         // длина события после трима хвостовых \r\n
};

inline bool parse_event_header(const char* ev, std::size_t n, EventHeader& h) {
    // Хвостовые \r\n события обрезаются (внутренние сохраняются), §2.1
    while (n > 0 && (ev[n - 1] == '\n' || ev[n - 1] == '\r')) --n;
    if (n == 0) return false;

    const char* comma = static_cast<const char*>(std::memchr(ev, ',', n));
    if (!comma) return false;
    const char* dash = static_cast<const char*>(
        std::memchr(ev, '-', static_cast<std::size_t>(comma - ev)));
    if (!dash) return false;

    h.time_b = ev;
    h.time_n = static_cast<std::size_t>(dash - ev);
    h.dur_b = dash + 1;
    h.dur_n = static_cast<std::size_t>(comma - h.dur_b);
    // Канонизация duration: без ведущих нулей, "000" → "0" (KI-2)
    while (h.dur_n > 1 && h.dur_b[0] == '0') {
        ++h.dur_b;
        --h.dur_n;
    }

    std::size_t p = static_cast<std::size_t>(comma - ev) + 1;
    const char* c2 = static_cast<const char*>(std::memchr(ev + p, ',', n - p));
    if (!c2) {
        // Нет второй запятой после имени события → parse_skip (§6)
        return false;
    }
    h.name_b = ev + p;
    h.name_n = static_cast<std::size_t>(c2 - h.name_b);
    p = static_cast<std::size_t>(c2 - ev) + 1;

    // Уровень — до следующей запятой; если её нет, level съедает весь остаток
    // события и свойства не разбираются (golden-кейс short_header).
    h.lvl_b = ev + p;
    const char* c3 = static_cast<const char*>(std::memchr(ev + p, ',', n - p));
    if (c3) {
        h.lvl_n = static_cast<std::size_t>(c3 - h.lvl_b);
        p = static_cast<std::size_t>(c3 - ev) + 1;
    } else {
        h.lvl_n = n - p;
        p = n;
    }
    h.props_off = p;
    h.n = n;
    return true;
}

// --- Автомат свойств Имя=Значение (format-spec §3, §4) ------------------------
// Имя до '=', значение по правилам кавычек §4.1 либо без кавычек до ','.
// Хвост без '=' молча отбрасывается. Управляющая логика ЕДИНА для NDJSON и
// RowBinary; различается только эмиссия (Policy):
//   key(s,n)          — имя свойства;
//   empty_value()     — '=' в самом конце события (пустое значение);
//   val_begin()       — начало значения в кавычках;
//   val_raw(s,n)      — сегмент данных значения;
//   val_quote(c)      — кавычка-«данные» внутри значения ('' / "" / KI-10);
//   val_end()         — конец значения в кавычках;
//   val_unquoted(name,name_n,v,n) — значение без кавычек (типизация — дело политики).
template <class Policy>
inline void append_props_impl(Policy& pol, const char* ev, std::size_t p, std::size_t end) {
    while (p < end) {
        const void* eqp = std::memchr(ev + p, '=', end - p);
        if (!eqp) break;
        std::size_t eq_pos = static_cast<std::size_t>(static_cast<const char*>(eqp) - ev);
        const char* name = ev + p;
        std::size_t name_n = eq_pos - p;

        pol.key(name, name_n);

        p = eq_pos + 1;
        if (p >= end) {
            pol.empty_value();
            break;
        }

        char q = ev[p];
        if (q == '\'') {
            // Одинарные кавычки: '' — экранирование; одиночная ' закрывает
            // значение только перед ',' или концом события (KI-10).
            pol.val_begin();
            ++p;
            std::size_t val_start = p;
            bool closed = false;
            while (p < end) {
                const void* qp = std::memchr(ev + p, '\'', end - p);
                if (!qp) {
                    pol.val_raw(ev + val_start, end - val_start);
                    pol.val_end();
                    p = end;
                    closed = true;
                    break;
                }
                p = static_cast<std::size_t>(static_cast<const char*>(qp) - ev);
                if (p + 1 < end && ev[p + 1] == '\'') {
                    // Экранирование '' → одна кавычка в данных
                    pol.val_raw(ev + val_start, p - val_start);
                    pol.val_quote('\'');
                    p += 2;
                    val_start = p;
                } else if (p + 1 == end || ev[p + 1] == ',') {
                    // Закрывающая кавычка
                    pol.val_raw(ev + val_start, p - val_start);
                    pol.val_end();
                    ++p;
                    closed = true;
                    break;
                } else {
                    // Битый формат: одиночная ' внутри — считаем данными
                    pol.val_raw(ev + val_start, p - val_start);
                    pol.val_quote('\'');
                    ++p;
                    val_start = p;
                }
            }
            if (!closed) {
                pol.val_raw(ev + val_start, p - val_start);
                pol.val_end();
            }
        } else if (q == '"') {
            // Двойные кавычки: "" — экранирование; первая одиночная "
            // закрывает безусловно (несимметрия зафиксирована в §4.1).
            pol.val_begin();
            ++p;
            std::size_t val_start = p;
            bool closed = false;
            while (p < end) {
                const void* qp = std::memchr(ev + p, '"', end - p);
                if (!qp) {
                    pol.val_raw(ev + val_start, end - val_start);
                    pol.val_end();
                    p = end;
                    closed = true;
                    break;
                }
                p = static_cast<std::size_t>(static_cast<const char*>(qp) - ev);
                if (p + 1 < end && ev[p + 1] == '"') {
                    pol.val_raw(ev + val_start, p - val_start);
                    pol.val_quote('"');
                    p += 2;
                    val_start = p;
                    continue;
                }
                pol.val_raw(ev + val_start, p - val_start);
                pol.val_end();
                ++p;
                closed = true;
                break;
            }
            if (!closed) {
                pol.val_raw(ev + val_start, p - val_start);
                pol.val_end();
            }
        } else {
            // Без кавычек: до ',' или конца события.
            std::size_t sep = end;
            const void* cp = std::memchr(ev + p, ',', end - p);
            if (cp) sep = static_cast<std::size_t>(static_cast<const char*>(cp) - ev);
            pol.val_unquoted(name, name_n, ev + p, sep - p);
            p = sep;
        }

        if (p < end && ev[p] == ',') ++p;
    }
}

// Политика NDJSON: побайтно повторяет историческую эмиссию (golden-гейт
// сравнивает вывод с эталоном побайтно).
struct NdjsonProps {
    std::string& dst;
    void key(const char* s, std::size_t n) {
        dst.append(",\"", 2);
        append_escaped(dst, s, n);
        dst.append("\":", 2);
    }
    void empty_value() { dst.append("\"\"", 2); }
    void val_begin() { dst.push_back('"'); }
    void val_raw(const char* s, std::size_t n) { append_escaped(dst, s, n); }
    void val_quote(char c) {
        if (c == '"') {
            dst.append("\\\"", 2);
        } else {
            dst.push_back(c);
        }
    }
    void val_end() { dst.push_back('"'); }
    void val_unquoted(const char* name, std::size_t name_n, const char* v, std::size_t n) {
        // Число по строгой грамматике, кроме always-string полей (§4.2).
        if (!is_always_string_field(name, name_n) && is_number_token(v, n)) {
            dst.append(v, n);
        } else {
            dst.push_back('"');
            append_escaped(dst, v, n);
            dst.push_back('"');
        }
    }
};

// --- Примитивы RowBinary ------------------------------------------------------

// UVarInt ClickHouse (беззнаковый LEB128).
inline void append_varint(std::string& dst, std::uint64_t v) {
    while (v >= 0x80) {
        dst.push_back(static_cast<char>(v | 0x80));
        v >>= 7;
    }
    dst.push_back(static_cast<char>(v));
}

inline void append_le64(std::string& dst, std::uint64_t v) {
    char b[8];
    for (int i = 0; i < 8; ++i) b[i] = static_cast<char>(v >> (8 * i));
    dst.append(b, 8);
}

inline void append_rb_string(std::string& dst, const char* s, std::size_t n) {
    append_varint(dst, n);
    dst.append(s, n);
}

// Дни от эпохи Unix по гражданскому календарю (алгоритм Хауарда Хиннанта).
// Для «месяца 13» и прочих невалидированных значений результат детерминирован
// (перетекает в соседний период), UB нет.
inline std::int64_t days_from_civil(std::int64_t y, unsigned m, unsigned d) {
    y -= m <= 2;
    const std::int64_t era = (y >= 0 ? y : y - 399) / 400;
    const unsigned yoe = static_cast<unsigned>(y - era * 400);
    const unsigned doy = (153u * (m + (m > 2 ? -3 : 9)) + 2u) / 5u + d - 1u;
    const unsigned doe = yoe * 365u + yoe / 4u - yoe / 100u + doy;
    return era * 146097 + static_cast<std::int64_t>(doe) - 719468;
}

// Политика RowBinary: пары Map копятся в ctx.pairs (число пар должно быть
// записано ПЕРЕД парами), значение — в ctx.val (длина строки — перед байтами).
struct RbProps {
    RowBinaryCtx& ctx;
    std::uint64_t count = 0;
    void key(const char* s, std::size_t n) {
        ++count;
        append_rb_string(ctx.pairs, s, n);
    }
    void empty_value() { append_varint(ctx.pairs, 0); }
    void val_begin() { ctx.val.clear(); }
    void val_raw(const char* s, std::size_t n) { ctx.val.append(s, n); }
    void val_quote(char c) { ctx.val.push_back(c); }
    void val_end() { append_rb_string(ctx.pairs, ctx.val.data(), ctx.val.size()); }
    void val_unquoted(const char*, std::size_t, const char* v, std::size_t n) {
        // В ClickHouse значения свойств — всегда текст: сырой токен как есть.
        append_rb_string(ctx.pairs, v, n);
    }
};

} // namespace

bool append_event(std::string& dst, const char* ev, std::size_t n,
                  const std::string& date_prefix,
                  const std::string& filename_esc,
                  const std::string& file_path_esc) {
    EventHeader h;
    if (!parse_event_header(ev, n, h)) return false;

    dst.append("{\"timestamp\":\"", 14);
    dst.append(date_prefix);
    dst.append(h.time_b, h.time_n); // маска гарантирует только цифры/':'/'.'
    dst.append("\",\"duration\":", 13);
    dst.append(h.dur_b, h.dur_n);
    dst.append(",\"event\":\"", 10);
    append_escaped(dst, h.name_b, h.name_n);
    dst.append("\",\"level\":", 10);
    if (is_number_token(h.lvl_b, h.lvl_n)) {
        dst.append(h.lvl_b, h.lvl_n);
    } else {
        dst.push_back('"');
        append_escaped(dst, h.lvl_b, h.lvl_n);
        dst.push_back('"');
    }
    dst.append(",\"filename\":\"", 13);
    dst.append(filename_esc);
    dst.append("\",\"file_path\":\"", 15);
    dst.append(file_path_esc);
    dst.push_back('"');

    NdjsonProps pol{dst};
    append_props_impl(pol, ev, h.props_off, h.n);

    dst.append("}\n", 2);
    return true;
}

void rb_init_date(RowBinaryCtx& ctx, const std::string& date_prefix) {
    // date_prefix — "20YY-MM-DDTHH:" (ровно 14 символов, цифры на известных
    // позициях — см. util::date_from_filename) либо пуст (деградированный файл).
    if (date_prefix.size() != 14) {
        ctx.date_us = -1;
        return;
    }
    const char* p = date_prefix.data();
    auto d2 = [&](int off) { return (p[off] - '0') * 10 + (p[off + 1] - '0'); };
    std::int64_t year = (p[0] - '0') * 1000 + (p[1] - '0') * 100 + d2(2);
    unsigned month = static_cast<unsigned>(d2(5));
    unsigned day = static_cast<unsigned>(d2(8));
    std::int64_t hour = d2(11);
    std::int64_t days = days_from_civil(year, month, day);
    ctx.date_us = (days * 86400 + hour * 3600) * 1000000;
}

bool append_event_rowbinary(std::string& dst, const char* ev, std::size_t n,
                            RowBinaryCtx& ctx) {
    EventHeader h;
    if (!parse_event_header(ev, n, h)) return false;

    // timestamp DateTime64(6): Int64 LE, µs с эпохи. Маска события гарантирует
    // время "ММ:СС.мммммм" (12 байт); деградированный файл (нет даты) → 0.
    std::int64_t ts = 0;
    if (ctx.date_us >= 0) {
        ts = ctx.date_us;
        if (h.time_n == 12) {
            const char* t = h.time_b;
            std::int64_t mins = (t[0] - '0') * 10 + (t[1] - '0');
            std::int64_t secs = (t[3] - '0') * 10 + (t[4] - '0');
            std::int64_t us = 0;
            for (int i = 6; i < 12; ++i) us = us * 10 + (t[i] - '0');
            ts += (mins * 60 + secs) * 1000000 + us;
        }
    }
    append_le64(dst, static_cast<std::uint64_t>(ts));

    // duration UInt64 LE; переполнение (не встречается на корпусе) клампится.
    std::uint64_t dur = 0;
    for (std::size_t i = 0; i < h.dur_n; ++i) {
        unsigned digit = static_cast<unsigned>(h.dur_b[i] - '0');
        if (dur > (UINT64_MAX - digit) / 10) {
            dur = UINT64_MAX;
            break;
        }
        dur = dur * 10 + digit;
    }
    append_le64(dst, dur);

    append_rb_string(dst, h.name_b, h.name_n);                       // event
    append_rb_string(dst, h.lvl_b, h.lvl_n);                         // level (текст)
    append_rb_string(dst, ctx.filename.data(), ctx.filename.size()); // filename
    append_rb_string(dst, ctx.file_path.data(), ctx.file_path.size()); // file_path

    // props Map(String,String): число пар — ПЕРЕД парами, поэтому пары
    // сначала копятся в scratch.
    ctx.pairs.clear();
    RbProps pol{ctx};
    append_props_impl(pol, ev, h.props_off, h.n);
    append_varint(dst, pol.count);
    dst.append(ctx.pairs);
    return true;
}

} // namespace parse
} // namespace tj
