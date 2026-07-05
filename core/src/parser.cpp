// parser.cpp — разрезание на события, автомат свойств, эмиссия JSON.
// Семантика: docs/format-spec.md v1.0 rev 3, побайтная совместимость с эталоном.
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

// Автомат свойств Имя=Значение (format-spec §3, §4): имя до '=', значение по
// правилам кавычек §4.1 либо без кавычек до ',' с типизацией §4.2.
// Хвост без '=' молча отбрасывается.
void append_props(std::string& dst, const char* ev, std::size_t p, std::size_t end) {
    while (p < end) {
        const void* eqp = std::memchr(ev + p, '=', end - p);
        if (!eqp) break;
        std::size_t eq_pos = static_cast<std::size_t>(static_cast<const char*>(eqp) - ev);
        const char* name = ev + p;
        std::size_t name_n = eq_pos - p;

        dst.append(",\"", 2);
        append_escaped(dst, name, name_n);
        dst.append("\":", 2);

        p = eq_pos + 1;
        if (p >= end) {
            dst.append("\"\"", 2);
            break;
        }

        char q = ev[p];
        if (q == '\'') {
            // Одинарные кавычки: '' — экранирование; одиночная ' закрывает
            // значение только перед ',' или концом события (KI-10).
            dst.push_back('"');
            ++p;
            std::size_t val_start = p;
            bool closed = false;
            while (p < end) {
                const void* qp = std::memchr(ev + p, '\'', end - p);
                if (!qp) {
                    append_escaped(dst, ev + val_start, end - val_start);
                    dst.push_back('"');
                    p = end;
                    closed = true;
                    break;
                }
                p = static_cast<std::size_t>(static_cast<const char*>(qp) - ev);
                if (p + 1 < end && ev[p + 1] == '\'') {
                    // Экранирование '' → одна кавычка в данных
                    append_escaped(dst, ev + val_start, p - val_start);
                    dst.push_back('\'');
                    p += 2;
                    val_start = p;
                } else if (p + 1 == end || ev[p + 1] == ',') {
                    // Закрывающая кавычка
                    append_escaped(dst, ev + val_start, p - val_start);
                    dst.push_back('"');
                    ++p;
                    closed = true;
                    break;
                } else {
                    // Битый формат: одиночная ' внутри — считаем данными
                    append_escaped(dst, ev + val_start, p - val_start);
                    dst.push_back('\'');
                    ++p;
                    val_start = p;
                }
            }
            if (!closed) {
                append_escaped(dst, ev + val_start, p - val_start);
                dst.push_back('"');
            }
        } else if (q == '"') {
            // Двойные кавычки: "" — экранирование; первая одиночная "
            // закрывает безусловно (несимметрия зафиксирована в §4.1).
            dst.push_back('"');
            ++p;
            std::size_t val_start = p;
            bool closed = false;
            while (p < end) {
                const void* qp = std::memchr(ev + p, '"', end - p);
                if (!qp) {
                    append_escaped(dst, ev + val_start, end - val_start);
                    dst.push_back('"');
                    p = end;
                    closed = true;
                    break;
                }
                p = static_cast<std::size_t>(static_cast<const char*>(qp) - ev);
                if (p + 1 < end && ev[p + 1] == '"') {
                    append_escaped(dst, ev + val_start, p - val_start);
                    dst.append("\\\"", 2);
                    p += 2;
                    val_start = p;
                    continue;
                }
                append_escaped(dst, ev + val_start, p - val_start);
                dst.push_back('"');
                ++p;
                closed = true;
                break;
            }
            if (!closed) {
                append_escaped(dst, ev + val_start, p - val_start);
                dst.push_back('"');
            }
        } else {
            // Без кавычек: до ',' или конца события; число по строгой
            // грамматике, кроме always-string полей (§4.2).
            std::size_t sep = end;
            const void* cp = std::memchr(ev + p, ',', end - p);
            if (cp) sep = static_cast<std::size_t>(static_cast<const char*>(cp) - ev);
            const char* val = ev + p;
            std::size_t val_n = sep - p;
            if (!is_always_string_field(name, name_n) && is_number_token(val, val_n)) {
                dst.append(val, val_n);
            } else {
                dst.push_back('"');
                append_escaped(dst, val, val_n);
                dst.push_back('"');
            }
            p = sep;
        }

        if (p < end && ev[p] == ',') ++p;
    }
}

} // namespace

bool append_event(std::string& dst, const char* ev, std::size_t n,
                  const std::string& date_prefix,
                  const std::string& filename_esc,
                  const std::string& file_path_esc) {
    // Хвостовые \r\n события обрезаются (внутренние сохраняются), §2.1
    while (n > 0 && (ev[n - 1] == '\n' || ev[n - 1] == '\r')) --n;
    if (n == 0) return false;

    // Заголовок: ММ:СС.мммммм-Длительность,Событие,Уровень[,...] (§2.2)
    const char* comma = static_cast<const char*>(std::memchr(ev, ',', n));
    if (!comma) return false;
    const char* dash = static_cast<const char*>(
        std::memchr(ev, '-', static_cast<std::size_t>(comma - ev)));
    if (!dash) return false;

    const char* time_b = ev;
    std::size_t time_n = static_cast<std::size_t>(dash - ev);
    const char* dur_b = dash + 1;
    std::size_t dur_n = static_cast<std::size_t>(comma - dur_b);
    // Канонизация duration: без ведущих нулей, "000" → "0" (KI-2)
    while (dur_n > 1 && dur_b[0] == '0') {
        ++dur_b;
        --dur_n;
    }

    std::size_t p = static_cast<std::size_t>(comma - ev) + 1;
    const char* c2 = static_cast<const char*>(std::memchr(ev + p, ',', n - p));
    if (!c2) {
        // Нет второй запятой после имени события → parse_skip (§6)
        return false;
    }
    const char* name_b = ev + p;
    std::size_t name_n = static_cast<std::size_t>(c2 - name_b);
    p = static_cast<std::size_t>(c2 - ev) + 1;

    // Уровень — до следующей запятой; если её нет, level съедает весь остаток
    // события и свойства не разбираются (golden-кейс short_header).
    const char* lvl_b = ev + p;
    std::size_t lvl_n;
    const char* c3 = static_cast<const char*>(std::memchr(ev + p, ',', n - p));
    if (c3) {
        lvl_n = static_cast<std::size_t>(c3 - lvl_b);
        p = static_cast<std::size_t>(c3 - ev) + 1;
    } else {
        lvl_n = n - p;
        p = n;
    }

    dst.append("{\"timestamp\":\"", 14);
    dst.append(date_prefix);
    dst.append(time_b, time_n); // маска гарантирует только цифры/':'/'.'
    dst.append("\",\"duration\":", 13);
    dst.append(dur_b, dur_n);
    dst.append(",\"event\":\"", 10);
    append_escaped(dst, name_b, name_n);
    dst.append("\",\"level\":", 10);
    if (is_number_token(lvl_b, lvl_n)) {
        dst.append(lvl_b, lvl_n);
    } else {
        dst.push_back('"');
        append_escaped(dst, lvl_b, lvl_n);
        dst.push_back('"');
    }
    dst.append(",\"filename\":\"", 13);
    dst.append(filename_esc);
    dst.append("\",\"file_path\":\"", 15);
    dst.append(file_path_esc);
    dst.push_back('"');

    append_props(dst, ev, p, n);

    dst.append("}\n", 2);
    return true;
}

} // namespace parse
} // namespace tj
