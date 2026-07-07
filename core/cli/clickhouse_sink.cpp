// clickhouse_sink.cpp — RowBinary over HTTP в ClickHouse через WinHTTP.
// Формат тела: подряд идущие RowBinary-строки (кодирует ядро, см. parser.hpp).
// Каждый батч — отдельный POST с Content-Length (WinHTTP держит keep-alive).
#include "clickhouse_sink.hpp"

#include <chrono>
#include <stdexcept>

#ifdef _WIN32
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <winhttp.h>
#endif

namespace tj_cli {

namespace {

long long steady_ms() {
    return std::chrono::duration_cast<std::chrono::milliseconds>(
               std::chrono::steady_clock::now().time_since_epoch())
        .count();
}

// Валидация имени "<db>.<table>": [A-Za-z0-9_] и ровно одна точка —
// исключает необходимость квотирования идентификаторов в тексте запроса.
bool valid_table_name(const std::string& t) {
    int dots = 0;
    if (t.empty() || t.front() == '.' || t.back() == '.') return false;
    for (char c : t) {
        if (c == '.') {
            ++dots;
        } else if (!(c == '_' || (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') ||
                     (c >= 'a' && c <= 'z'))) {
            return false;
        }
    }
    return dots == 1;
}

#ifdef _WIN32
std::wstring widen_ascii(const std::string& s) {
    std::wstring w;
    w.reserve(s.size());
    for (char c : s) w.push_back(static_cast<wchar_t>(static_cast<unsigned char>(c)));
    return w;
}

std::string win_error_text(DWORD code) {
    switch (code) {
        case ERROR_WINHTTP_CANNOT_CONNECT:
            return "нет соединения (сервер не слушает порт?)";
        case ERROR_WINHTTP_TIMEOUT:
            return "таймаут";
        case ERROR_WINHTTP_NAME_NOT_RESOLVED:
            return "имя хоста не разрешается";
        case ERROR_WINHTTP_CONNECTION_ERROR:
            return "соединение разорвано";
        default:
            return "код WinHTTP " + std::to_string(code);
    }
}
#endif

} // namespace

bool parse_clickhouse_dsn(const std::string& dsn, ClickHouseConfig& out, std::string& err) {
    if (dsn.empty()) return true; // дефолты: http://localhost:8123, tj_bench.events

    const std::string scheme = "http://";
    if (dsn.rfind(scheme, 0) != 0) {
        err = "DSN ClickHouse должен начинаться с http:// (получено \"" + dsn + "\")";
        return false;
    }
    std::string rest = dsn.substr(scheme.size());

    std::string hostport = rest;
    std::string table_part;
    std::size_t slash = rest.find('/');
    if (slash != std::string::npos) {
        hostport = rest.substr(0, slash);
        table_part = rest.substr(slash + 1);
    }

    if (hostport.empty()) {
        err = "в DSN ClickHouse пустой хост: \"" + dsn + "\"";
        return false;
    }
    std::size_t colon = hostport.rfind(':');
    if (colon != std::string::npos) {
        out.host = hostport.substr(0, colon);
        std::string port_s = hostport.substr(colon + 1);
        if (port_s.empty() || port_s.size() > 5) {
            err = "неверный порт в DSN ClickHouse: \"" + dsn + "\"";
            return false;
        }
        unsigned long port = 0;
        for (char c : port_s) {
            if (c < '0' || c > '9') {
                err = "неверный порт в DSN ClickHouse: \"" + dsn + "\"";
                return false;
            }
            port = port * 10 + static_cast<unsigned long>(c - '0');
        }
        if (port < 1 || port > 65535) {
            err = "порт в DSN ClickHouse должен быть 1..65535: \"" + dsn + "\"";
            return false;
        }
        out.port = static_cast<unsigned>(port);
    } else {
        out.host = hostport;
    }
    if (out.host.empty()) {
        err = "в DSN ClickHouse пустой хост: \"" + dsn + "\"";
        return false;
    }

    if (!table_part.empty()) {
        // Путь настраивает целевую таблицу: /<db>.<table>
        if (!valid_table_name(table_part)) {
            err = "путь DSN ClickHouse должен иметь вид <db>.<table> "
                  "(символы [A-Za-z0-9_], одна точка): \"" + table_part + "\"";
            return false;
        }
        out.table = table_part;
    }
    return true;
}

#ifdef _WIN32

ClickHouseSink::ClickHouseSink(ClickHouseConfig cfg) : cfg_(std::move(cfg)) {
    if (!valid_table_name(cfg_.table)) {
        throw std::runtime_error("недопустимое имя таблицы ClickHouse: " + cfg_.table);
    }
    // Текст запроса фиксирован; кодируем только пробелы (%20) — остальные
    // символы после валидации таблицы URL-безопасны.
    std::string query = "INSERT INTO " + cfg_.table + " FORMAT RowBinary";
    std::string path = "/?query=";
    for (char c : query) {
        if (c == ' ') {
            path += "%20";
        } else {
            path.push_back(c);
        }
    }
    path_ = widen_ascii(path);
    whost_ = widen_ascii(cfg_.host);

    session_ = WinHttpOpen(L"tj-agent-cpp/1.0", WINHTTP_ACCESS_TYPE_NO_PROXY,
                           WINHTTP_NO_PROXY_NAME, WINHTTP_NO_PROXY_BYPASS, 0);
    if (!session_) {
        throw std::runtime_error("WinHttpOpen: " + win_error_text(GetLastError()));
    }
    // Быстрый отказ на неверном порту/хосте вместо зависания: resolve/connect
    // 5 с; send/receive 120 с (вставка 64-МБ батча укладывается с запасом).
    WinHttpSetTimeouts(static_cast<HINTERNET>(session_), 5000, 5000, 120000, 120000);

    connect_ = WinHttpConnect(static_cast<HINTERNET>(session_), whost_.c_str(),
                              static_cast<INTERNET_PORT>(cfg_.port), 0);
    if (!connect_) {
        DWORD e = GetLastError();
        WinHttpCloseHandle(static_cast<HINTERNET>(session_));
        session_ = nullptr;
        throw std::runtime_error("WinHttpConnect: " + win_error_text(e));
    }

    batch_.reserve(static_cast<std::size_t>(
        cfg_.batch_bytes < (256ull << 20) ? cfg_.batch_bytes + (8u << 20) : (64ull << 20)));
    last_flush_ms_ = steady_ms();

    // Префлайт: INSERT с пустым телом — валидный no-op; проверяет доступность
    // сервера, БД и таблицы до начала разбора корпуса.
    post(nullptr, 0);
}

ClickHouseSink::~ClickHouseSink() {
    if (connect_) WinHttpCloseHandle(static_cast<HINTERNET>(connect_));
    if (session_) WinHttpCloseHandle(static_cast<HINTERNET>(session_));
}

void ClickHouseSink::post(const char* body, std::size_t len) {
    HINTERNET req = WinHttpOpenRequest(static_cast<HINTERNET>(connect_), L"POST", path_.c_str(),
                                       nullptr, WINHTTP_NO_REFERER,
                                       WINHTTP_DEFAULT_ACCEPT_TYPES, 0);
    if (!req) {
        throw std::runtime_error("WinHttpOpenRequest: " + win_error_text(GetLastError()));
    }

    struct ReqGuard {
        HINTERNET h;
        ~ReqGuard() { WinHttpCloseHandle(h); }
    } guard{req};

    if (!WinHttpSendRequest(req, WINHTTP_NO_ADDITIONAL_HEADERS, 0, WINHTTP_NO_REQUEST_DATA, 0,
                            static_cast<DWORD>(len), 0)) {
        throw std::runtime_error("ClickHouse " + cfg_.host + ":" + std::to_string(cfg_.port) +
                                 " недоступен: " + win_error_text(GetLastError()));
    }
    // Тело — по частям (WinHTTP принимает DWORD за вызов; батч ≤ 64 МБ + чанк).
    std::size_t off = 0;
    while (off < len) {
        DWORD piece = static_cast<DWORD>(
            (len - off) < (16u << 20) ? (len - off) : (16u << 20));
        DWORD written = 0;
        if (!WinHttpWriteData(req, body + off, piece, &written) || written == 0) {
            throw std::runtime_error("WinHttpWriteData: " + win_error_text(GetLastError()));
        }
        off += written;
    }
    if (!WinHttpReceiveResponse(req, nullptr)) {
        throw std::runtime_error("WinHttpReceiveResponse: " + win_error_text(GetLastError()));
    }

    DWORD status = 0;
    DWORD status_size = sizeof(status);
    if (!WinHttpQueryHeaders(req, WINHTTP_QUERY_STATUS_CODE | WINHTTP_QUERY_FLAG_NUMBER,
                             WINHTTP_HEADER_NAME_BY_INDEX, &status, &status_size,
                             WINHTTP_NO_HEADER_INDEX)) {
        throw std::runtime_error("WinHttpQueryHeaders: " + win_error_text(GetLastError()));
    }

    // Тело ответа читается всегда (обязательное условие keep-alive);
    // при ошибке оно же — текст исключения ClickHouse.
    std::string response;
    for (;;) {
        DWORD avail = 0;
        if (!WinHttpQueryDataAvailable(req, &avail) || avail == 0) break;
        std::size_t old = response.size();
        if (old >= (64u << 10)) { // хвост гигантского ответа не нужен
            char sink_buf[4096];
            DWORD rd = 0;
            if (!WinHttpReadData(req, sink_buf, sizeof(sink_buf), &rd) || rd == 0) break;
            continue;
        }
        response.resize(old + avail);
        DWORD rd = 0;
        if (!WinHttpReadData(req, &response[old], avail, &rd)) break;
        response.resize(old + rd);
        if (rd == 0) break;
    }

    if (status != 200) {
        throw std::runtime_error("ClickHouse вернул HTTP " + std::to_string(status) + ": " +
                                 (response.empty() ? "<пустой ответ>" : response));
    }
}

void ClickHouseSink::flush() {
    if (batch_.empty() && batch_rows_ == 0) {
        last_flush_ms_ = steady_ms();
        return;
    }
    post(batch_.data(), batch_.size());
    inserted_ += batch_rows_;
    batch_.clear();
    batch_rows_ = 0;
    last_flush_ms_ = steady_ms();
}

void ClickHouseSink::append(const char* data, std::size_t len, std::uint64_t rows) {
    batch_.append(data, len);
    batch_rows_ += rows;
    if (batch_rows_ >= cfg_.batch_rows || batch_.size() >= cfg_.batch_bytes ||
        static_cast<std::uint64_t>(steady_ms() - last_flush_ms_) >= cfg_.flush_ms) {
        flush();
    }
}

void ClickHouseSink::finish() { flush(); }

#else // !_WIN32

// POSIX-порт ядра компилируется, но ClickHouse-клиент пока Windows-only
// (WinHTTP). При необходимости — raw-socket HTTP/1.1 по тому же контракту.
ClickHouseSink::ClickHouseSink(ClickHouseConfig cfg) : cfg_(std::move(cfg)) {
    throw std::runtime_error("--sink clickhouse поддерживается только в Windows-сборке");
}
ClickHouseSink::~ClickHouseSink() = default;
void ClickHouseSink::post(const char*, std::size_t) {}
void ClickHouseSink::flush() {}
void ClickHouseSink::append(const char*, std::size_t, std::uint64_t) {}
void ClickHouseSink::finish() {}

#endif

} // namespace tj_cli
