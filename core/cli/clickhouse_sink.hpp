// clickhouse_sink.hpp — приёмник RowBinary-чанков конвейера в ClickHouse по
// HTTP-интерфейсу (POST /?query=INSERT ... FORMAT RowBinary).
//
// Ноль внешних зависимостей: клиент — WinHTTP (системная библиотека Windows).
// Кодирование строк выполняет ядро (tj::parse::append_event_rowbinary) на
// уровне разобранного события — JSON не пересобирается и не перечитывается.
//
// Батч-политика: флаш при достижении batch_rows ИЛИ batch_bytes ИЛИ flush_ms
// с момента прошлого флаша (проверяется при поступлении чанка: в batch-режиме
// поток чанков непрерывен, отдельный поток-таймер не нужен; финальный остаток
// отправляет finish()).
//
// Все методы вызываются на одном потоке (писатель конвейера). Ошибки HTTP /
// ClickHouse — std::runtime_error с телом ответа сервера.
#pragma once

#include <cstddef>
#include <cstdint>
#include <string>

namespace tj_cli {

struct ClickHouseConfig {
    std::string host = "localhost";
    unsigned port = 8123;                       // HTTP-интерфейс
    std::string table = "tj_bench.events";      // "<db>.<table>"
    std::uint64_t batch_rows = 50000;
    std::uint64_t batch_bytes = 64ull << 20;
    std::uint64_t flush_ms = 1000;
};

// Разбор DSN значения "--sink clickhouse[:<url>]".
// "" → дефолты (http://localhost:8123, tj_bench.events);
// иначе: http://host[:port][/<db>.<table>] (путь настраивает целевую таблицу).
// false → err заполнен (сообщение по-русски, как остальной CLI).
bool parse_clickhouse_dsn(const std::string& dsn, ClickHouseConfig& out, std::string& err);

class ClickHouseSink {
public:
    // Открывает HTTP-сессию и делает префлайт: INSERT с пустым телом (no-op)
    // проверяет доступность сервера и существование таблицы ДО начала разбора.
    // Бросает std::runtime_error (неверный порт/хост — быстрый отказ, не зависание).
    explicit ClickHouseSink(ClickHouseConfig cfg);
    ~ClickHouseSink();
    ClickHouseSink(const ClickHouseSink&) = delete;
    ClickHouseSink& operator=(const ClickHouseSink&) = delete;

    // Чанк целых RowBinary-строк от конвейера. Бросает при ошибке вставки.
    void append(const char* data, std::size_t len, std::uint64_t rows);
    // Финальный флаш остатка. Бросает при ошибке вставки.
    void finish();

    // Для tail-режима (--follow): немедленный флаш накопленного батча.
    // При ошибке БРОСАЕТ, батч сохраняется (post не очищает буфер до успеха) —
    // вызывающий ретраит с бэкоффом; успешный HTTP 200 и есть ack батча.
    void flush_pending() { flush(); }
    // Неподтверждённый остаток (строк/байт в накопленном батче).
    std::uint64_t pending_rows() const { return batch_rows_; }
    std::size_t pending_bytes() const { return batch_.size(); }

    std::uint64_t inserted_rows() const { return inserted_; }

private:
    void flush();                                  // отправка накопленного батча
    void post(const char* body, std::size_t len);  // один INSERT-POST

    ClickHouseConfig cfg_;
    std::wstring path_;         // /?query=INSERT%20INTO%20<db>.<table>%20FORMAT%20RowBinary
    std::wstring whost_;
    void* session_ = nullptr;   // HINTERNET (void*: без <windows.h> в заголовке)
    void* connect_ = nullptr;
    std::string batch_;
    std::uint64_t batch_rows_ = 0;
    std::uint64_t inserted_ = 0;
    long long last_flush_ms_ = 0; // steady_clock, мс
};

} // namespace tj_cli
