// parser.hpp — внутренние примитивы разбора ТЖ 1С (не публичный API).
//
// Байт-в-байт порт семантики cpp_parse/count_contexts.cpp по спецификации
// docs/format-spec.md v1.0 rev 3 (сверен с Go-агентом agents/go, который
// проходит golden-гейт). Любое отклонение — баг: golden-суита сравнивает
// вывод побайтно с замороженными эталонами.
#pragma once

#include <cstddef>
#include <cstring>
#include <string>

namespace tj {
namespace parse {

// Файлы короче пропускаются целиком (format-spec §6).
constexpr std::size_t kMinFileSize = 100;

// Маска начала события: ^\d{2}:\d{2}\.\d{6}-\d+, (format-spec §2.1).
// b — от начала физической строки до конца данных.
bool is_event_start(const char* b, std::size_t n);

// Строгая грамматика JSON-числа RFC 8259, длина ≤ 32 (format-spec §4.2, KI-2).
bool is_number_token(const char* v, std::size_t n);

// JSON-экранирование (format-spec §4.4): `"`, `\`, \b \f \n \r \t,
// прочие < 0x20 → \u00xx (hex в нижнем регистре); байты ≥ 0x20 как есть (KI-3).
void append_escaped(std::string& dst, const char* s, std::size_t n);

// Разбор одного события: дописывает в dst готовую JSON-строку с завершающим
// '\n'. false — событие отброшено (parse_skip: нет второй запятой и т.п., §6).
// filename_esc/file_path_esc — уже экранированные значения (общие на файл).
bool append_event(std::string& dst, const char* ev, std::size_t n,
                  const std::string& date_prefix,
                  const std::string& filename_esc,
                  const std::string& file_path_esc);

// Разрезание содержимого файла на события по маске начала строки (§2.1).
// BOM в начале пропускается (KI-6); контент до первой строки-маски
// отбрасывается. Чётность кавычек НЕ проверяется — KI-1 воспроизводится
// сознательно (golden-кейс mask_inside_quotes остаётся XFAIL).
template <class Emit>
void split_events(const char* data, std::size_t n, Emit&& emit) {
    if (n >= 3 && static_cast<unsigned char>(data[0]) == 0xEF &&
        static_cast<unsigned char>(data[1]) == 0xBB &&
        static_cast<unsigned char>(data[2]) == 0xBF) {
        data += 3;
        n -= 3;
    }
    std::size_t ptr = 0;
    std::size_t event_start = 0;
    bool in_event = is_event_start(data, n);
    while (ptr < n) {
        const void* nl = std::memchr(data + ptr, '\n', n - ptr);
        if (!nl) break;
        ptr = static_cast<std::size_t>(static_cast<const char*>(nl) - data) + 1;
        if (ptr < n && is_event_start(data + ptr, n - ptr)) {
            if (in_event) emit(data + event_start, ptr - event_start);
            in_event = true;
            event_start = ptr;
        }
    }
    if (in_event && n - event_start > 0) emit(data + event_start, n - event_start);
}

} // namespace parse
} // namespace tj
