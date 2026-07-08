// follow.cpp — tail-режим (--follow): непрерывное слежение за каталогом,
// вставка в ClickHouse, чекпоинты «ровно после ack», нулевые потери.
//
// Модель — один поток-оркестратор (петля poll_ms):
//   1) стоп-файл появился → дренаж и выход 0;
//   2) дискавери: рекурсивный обход --input на новые *.log и рост известных
//      (подхват нового файла < 2 с при poll_ms=500); гейт MIN_FILE_SIZE=100
//      переоценивается по мере роста файла;
//   3) чтение выросших файлов буферизованным ReadFile с запомненных смещений
//      (файлы открыты с FILE_SHARE_READ|WRITE|DELETE — писатель 1С не страдает);
//   4) сборка событий по правилам закрытия: (1) пришла следующая строка-маска;
//      (2) хвост оканчивается '\n' и нет новых данных idle_close_ms;
//      (3) грациозный стоп — дренаж только \n-терминированного остатка.
//      Незавершённая строка (без '\n') НЕ эмитится никогда;
//   5) флаш батча в ClickHouse (пороги строк/байт/времени); HTTP 200 — ack;
//   6) чекпоинты двигаются ТОЛЬКО после ack (min-contiguous по файлу),
//      персист атомарен (tmp + MoveFileEx REPLACE).
//
// Гарантия: at-least-once (крэш между ack и персистом чекпоинта даёт
// небольшие дубли), потерь нет: committed_offset никогда не обгоняет ack.
// Бэкпрешер чтения естественный: петля однопоточна, ретраи sink блокируют
// чтение; батч ограничен порогами sink (batch_bytes + одно событие).
//
// Инвариант смещений (на файл): [0, parsed_off) отдано sink либо отброшено;
// [parsed_off, parsed_off + |open_event| ± BOM) — открытое событие;
// дальше — raw_buf (прочитано, не нарезано; хвост без '\n'); конец = read_off.
// Поэтому конец открытого события всегда = read_off - |raw_buf| — этим
// выражением пользуются idle-close/дренаж (BOM-байты, вырезанные из
// содержимого события, в смещениях учтены).
#include "follow.hpp"

#include <algorithm>
#include <cinttypes>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <exception>
#include <filesystem>
#include <map>
#include <memory>
#include <set>
#include <string>
#include <system_error>
#include <vector>

#include "../src/parser.hpp" // разбор ядра: is_event_start, append_event_rowbinary
#include "../src/util.hpp"   // date_from_filename, rel_file_path

#ifdef _WIN32
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#endif

namespace fs = std::filesystem;

namespace tj_cli {

#ifdef _WIN32

namespace {

// ---------------------------------------------------------------------------
// Внутренние пределы реализации (контрактные значения — poll_ms/idle_close_ms —
// приходят из FollowConfig).
constexpr std::size_t kReadChunk = 1u << 20;          // один ReadFile: 1 МБ
constexpr std::uint64_t kMaxReadPerTick = 8u << 20;   // на файл за итерацию: 8 МБ
constexpr std::size_t kOpenEventGuard = 512u << 20;   // страховка от «вечного» события
constexpr int kMaxFlushRetries = 8;                   // bounded retries флаша
constexpr std::uint64_t kBackoffStartMs = 1000;       // 1 с → …
constexpr std::uint64_t kBackoffCapMs = 30000;        // … → потолок 30 с
constexpr std::uint64_t kCheckpointSaveMs = 1000;     // персист dirty-чекпоинтов
constexpr std::uint64_t kProgressMs = 5000;           // прогресс в stderr
constexpr long long kReplaceProbeMs = 2000;           // сверка идентичности пути

long long steady_ms() {
    return std::chrono::duration_cast<std::chrono::milliseconds>(
               std::chrono::steady_clock::now().time_since_epoch())
        .count();
}

// ---------------------------------------------------------------------------
// Чекпоинты: state-файл tj-follow-checkpoints.v1, одна запись на файл-источник.
//
// Формат (UTF-8, LF; путь — до конца строки, экранирование не нужно: '\n' в
// путях Windows невозможен):
//   tj-follow-checkpoints v1
//   <volumeSerial hex> <fileIndex hex> <committed_offset dec> <абсолютный путь>
//
// Идентичность файла — dwVolumeSerialNumber + nFileIndexHigh/Low из
// GetFileInformationByHandle: ротация/пересоздание с тем же именем меняет
// index → рестарт начинает файл с нуля (контракт: size < offset ИЛИ смена
// идентичности → offset 0). Персист атомарный: временный файл +
// FlushFileBuffers + MoveFileEx(REPLACE_EXISTING | WRITE_THROUGH).
struct CheckpointEntry {
    std::uint64_t volume = 0;
    std::uint64_t index = 0;
    std::uint64_t offset = 0;
};

class CheckpointStore {
public:
    // false — state-каталог не создать (фатально для follow).
    bool init(const fs::path& state_dir, std::string& err) {
        std::error_code ec;
        fs::create_directories(state_dir, ec);
        if (ec) {
            err = "не удалось создать --state каталог " + state_dir.u8string() + ": " +
                  ec.message();
            return false;
        }
        file_ = state_dir / L"tj-follow-checkpoints.v1";
        tmp_ = state_dir / L"tj-follow-checkpoints.v1.tmp";
        load();
        return true;
    }

    const CheckpointEntry* find(const std::string& path_u8) const {
        auto it = map_.find(path_u8);
        return it == map_.end() ? nullptr : &it->second;
    }

    void put(const std::string& path_u8, const CheckpointEntry& e) {
        auto it = map_.find(path_u8);
        if (it != map_.end() && it->second.volume == e.volume && it->second.index == e.index &&
            it->second.offset == e.offset) {
            return; // без изменений — не дёргаем dirty
        }
        map_[path_u8] = e;
        dirty_ = true;
    }

    bool dirty() const { return dirty_; }

    // Атомарный персист; false — ошибка записи (не фатальна в моменте:
    // повторим на следующем тике; потерянный прогресс чекпоинта даёт лишь
    // дубли при рестарте, не потери).
    bool save() {
        if (!dirty_) return true;
        std::string body = "tj-follow-checkpoints v1\n";
        for (const auto& kv : map_) {
            char head[3 * 24];
            std::snprintf(head, sizeof(head), "%llx %llx %llu ",
                          static_cast<unsigned long long>(kv.second.volume),
                          static_cast<unsigned long long>(kv.second.index),
                          static_cast<unsigned long long>(kv.second.offset));
            body += head;
            body += kv.first;
            body += '\n';
        }
        HANDLE h = CreateFileW(tmp_.c_str(), GENERIC_WRITE, 0, nullptr, CREATE_ALWAYS,
                               FILE_ATTRIBUTE_NORMAL, nullptr);
        if (h == INVALID_HANDLE_VALUE) return false;
        DWORD written = 0;
        BOOL ok = WriteFile(h, body.data(), static_cast<DWORD>(body.size()), &written, nullptr);
        ok = ok && written == body.size();
        ok = FlushFileBuffers(h) && ok;
        CloseHandle(h);
        if (!ok) return false;
        if (!MoveFileExW(tmp_.c_str(), file_.c_str(),
                         MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH)) {
            return false;
        }
        dirty_ = false;
        return true;
    }

private:
    void load() {
        HANDLE h = CreateFileW(file_.c_str(), GENERIC_READ,
                               FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE, nullptr,
                               OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL, nullptr);
        if (h == INVALID_HANDLE_VALUE) return; // первый запуск — файла нет
        std::string data;
        char buf[64 * 1024];
        DWORD rd = 0;
        while (ReadFile(h, buf, sizeof(buf), &rd, nullptr) && rd > 0) data.append(buf, rd);
        CloseHandle(h);

        std::size_t pos = 0;
        bool first = true;
        while (pos < data.size()) {
            std::size_t nl = data.find('\n', pos);
            if (nl == std::string::npos) nl = data.size();
            std::string line = data.substr(pos, nl - pos);
            if (!line.empty() && line.back() == '\r') line.pop_back();
            pos = nl + 1;
            if (first) {
                first = false;
                if (line != "tj-follow-checkpoints v1") return; // чужой формат — с нуля
                continue;
            }
            if (line.empty()) continue;
            CheckpointEntry e;
            const char* p = line.c_str();
            char* end = nullptr;
            e.volume = std::strtoull(p, &end, 16);
            if (end == p || *end != ' ') continue;
            p = end + 1;
            e.index = std::strtoull(p, &end, 16);
            if (end == p || *end != ' ') continue;
            p = end + 1;
            e.offset = std::strtoull(p, &end, 10);
            if (end == p || *end != ' ') continue;
            std::string path(end + 1);
            if (path.empty()) continue;
            map_[path] = e;
        }
    }

    fs::path file_;
    fs::path tmp_;
    std::map<std::string, CheckpointEntry> map_;
    bool dirty_ = false;
};

// ---------------------------------------------------------------------------
// Один отслеживаемый файл.
struct TailFile {
    fs::path path;               // абсолютный путь
    std::string path_u8;         // он же UTF-8 (ключ чекпоинта)
    HANDLE h = INVALID_HANDLE_VALUE;
    std::uint64_t volume = 0;    // идентичность (GetFileInformationByHandle)
    std::uint64_t index = 0;

    std::uint64_t read_off = 0;      // следующий байт для ReadFile
    std::string raw_buf;             // прочитано, но не нарезано (хвост без '\n')
    std::string open_event;          // открытое событие: только ЦЕЛЫЕ строки
    std::uint64_t parsed_off = 0;    // всё до parsed_off отдано sink либо отброшено
    std::uint64_t committed_off = 0; // подтверждено ack (персистится)
    std::uint64_t rows_pending = 0;  // строк этого файла в неподтверждённом батче
    long long last_data_ms = 0;      // для правила idle-close
    long long last_probe_ms = 0;     // сверка идентичности пути (ротация)
    bool gated = false;              // < MIN_FILE_SIZE: ждём роста (размер — по handle!)

    tj::parse::RowBinaryCtx rbctx;   // filename/file_path/дата файла (как в batch)

    bool exists = true;              // путь виден в последнем дискавери
    std::uint64_t events = 0;        // телеметрия
    std::uint64_t parse_skips = 0;
};

// Исчерпание bounded-ретраев вставки: exit 1, чекпоинты по последнему ack.
struct FatalSinkError {
    std::string what;
};

// ---------------------------------------------------------------------------
// Оркестратор.
class Follower {
public:
    explicit Follower(const FollowConfig& cfg) : cfg_(cfg) {}

    ~Follower() {
        for (auto& kv : files_) {
            if (kv.second.h != INVALID_HANDLE_VALUE) CloseHandle(kv.second.h);
        }
    }

    int run() {
        const fs::path input = fs::u8path(cfg_.input);
        std::error_code ec;
        if (!fs::is_directory(input, ec) || ec) {
            std::fprintf(stderr, "Ошибка: --input не каталог: %s\n", cfg_.input.c_str());
            return 1;
        }
        input_abs_ = fs::absolute(input, ec);
        if (ec) input_abs_ = input;

        std::string err;
        if (!ckpt_.init(fs::u8path(cfg_.state_dir), err)) {
            std::fprintf(stderr, "Ошибка: %s\n", err.c_str());
            return 1;
        }
        stop_path_w_ = fs::u8path(cfg_.stop_file).wstring();

        // Sink: конструктор делает префлайт (сервер+таблица) — недоступный
        // ClickHouse на старте даёт быстрый отказ, а не тихое накопление.
        try {
            sink_ = std::make_unique<ClickHouseSink>(cfg_.ch);
        } catch (const std::exception& e) {
            std::fprintf(stderr, "Ошибка подключения к ClickHouse: %s\n", e.what());
            return 1;
        }
        last_inserted_ = sink_->inserted_rows();
        last_ok_flush_ms_ = steady_ms();
        last_progress_ms_ = steady_ms();
        last_ckpt_save_ms_ = steady_ms();

        std::fprintf(stderr,
                     "[follow] старт: input=%s, таблица=%s, poll=%llu мс, idle-close=%llu мс\n",
                     cfg_.input.c_str(), cfg_.ch.table.c_str(),
                     static_cast<unsigned long long>(cfg_.poll_ms),
                     static_cast<unsigned long long>(cfg_.idle_close_ms));

        int rc = 0;
        try {
            for (;;) {
                const long long tick_start = steady_ms();
                if (stop_requested()) {
                    drain_and_stop();
                    break;
                }
                discover();
                bool hot = false;
                for (auto& kv : files_) hot = poll_file(kv.second) || hot;
                idle_close_pass();
                flush_if_due();
                advance_rowless_commits();
                maybe_save_checkpoints(false);
                maybe_progress();
                reap_gone_files();
                if (!hot) {
                    const long long spent = steady_ms() - tick_start;
                    const long long budget = static_cast<long long>(cfg_.poll_ms);
                    if (spent < budget) Sleep(static_cast<DWORD>(budget - spent));
                }
            }
        } catch (const FatalSinkError& e) {
            std::fprintf(stderr,
                         "ОШИБКА: вставка в ClickHouse не удалась после %d попыток: %s\n",
                         kMaxFlushRetries, e.what.c_str());
            std::fprintf(stderr,
                         "[follow] чекпоинты зафиксированы по последнему ack — рестарт "
                         "продолжит без потерь\n");
            rc = 1;
        } catch (const std::exception& e) {
            std::fprintf(stderr, "Критическая ошибка follow: %s\n", e.what());
            rc = 1;
        }

        maybe_save_checkpoints(true);
        write_stats();
        progress_line(rc == 0 ? "финиш (exit 0)" : "аварийный финиш");
        return rc;
    }

private:
    // --- стоп-файл -----------------------------------------------------------
    bool stop_requested() const {
        return GetFileAttributesW(stop_path_w_.c_str()) != INVALID_FILE_ATTRIBUTES;
    }

    // --- дискавери -----------------------------------------------------------
    // Рекурсивный обход --input: каждый новый *.log берётся в слежение СРАЗУ
    // (открывается handle), смещение решает чекпоинт. Гейт MIN_FILE_SIZE=100
    // проверяется в poll_file по РАЗМЕРУ С HANDLE и переоценивается по мере
    // роста. Размер из перечисления каталога здесь не используется вовсе:
    // NTFS обновляет метаданные каталога для ОТКРЫТОГО писателем файла лениво
    // (реальный rphost держит .log открытым весь час) — по данным каталога
    // файл «заморожен» на нуле до закрытия, и агент простаивал бы до бёрста.
    void discover() {
        for (auto& kv : files_) kv.second.exists = false;
        last_walk_complete_ = false;

        std::error_code ec;
        fs::recursive_directory_iterator it(
            input_abs_, fs::directory_options::skip_permission_denied, ec);
        if (ec) {
            note_walk_error("Ошибка обхода " + input_abs_.u8string() + ": " + ec.message());
            return;
        }
        const fs::recursive_directory_iterator end_it;
        bool complete = true;
        while (it != end_it) {
            const fs::directory_entry& entry = *it;
            bool regular = entry.is_regular_file(ec);
            if (!ec && regular && entry.path().extension() == ".log") {
                const std::string key = entry.path().u8string();
                auto known = files_.find(key);
                if (known != files_.end()) {
                    known->second.exists = true;
                } else {
                    start_tracking(entry.path(), key);
                }
            }
            it.increment(ec);
            if (ec) {
                note_walk_error("Ошибка обхода каталогов: " + ec.message());
                complete = false;
                break;
            }
        }
        // Неполный обход не должен «терять» файлы: reap работает только после
        // полного (exists-флаги достоверны).
        last_walk_complete_ = complete;
    }

    void note_walk_error(const std::string& msg) {
        // Один и тот же сбой не долбим в stderr каждые poll_ms.
        if (walk_errors_seen_.insert(msg).second) {
            std::fprintf(stderr, "%s\n", msg.c_str());
            ++failed_files_;
        }
    }

    void start_tracking(const fs::path& p, const std::string& key) {
        TailFile f;
        f.path = p;
        f.path_u8 = key;
        if (!open_handle(f)) {
            // Транзиентно (гонка создания, антивирус): повторим на следующем
            // тике; ошибку логируем один раз на путь.
            if (open_errors_seen_.insert(key).second) {
                std::fprintf(stderr, "Ошибка открытия файла (повторим): %s\n", key.c_str());
                ++failed_files_;
            }
            return;
        }
        // Резюме по чекпоинту: идентичность совпала и файл не короче
        // committed_offset → продолжаем с него; иначе (ротация/усечение) — 0.
        const CheckpointEntry* ck = ckpt_.find(key);
        std::uint64_t resume = 0;
        LARGE_INTEGER sz{};
        GetFileSizeEx(f.h, &sz);
        const std::uint64_t fsize = static_cast<std::uint64_t>(sz.QuadPart);
        if (ck && ck->volume == f.volume && ck->index == f.index && ck->offset <= fsize) {
            resume = ck->offset;
        }
        f.read_off = f.parsed_off = f.committed_off = resume;
        f.last_data_ms = steady_ms();
        f.last_probe_ms = steady_ms();
        // Гейт MIN_FILE_SIZE: по размеру С HANDLE (точен и для открытого
        // писателем файла); резюме с offset > 0 означает, что файл уже
        // проходил гейт в прошлой жизни агента.
        f.gated = (resume == 0 && fsize < tj::parse::kMinFileSize);

        const std::string filename = p.filename().u8string();
        f.rbctx.filename = filename;
        f.rbctx.file_path = tj::util::rel_file_path(p);
        tj::parse::rb_init_date(f.rbctx, tj::util::date_from_filename(filename));

        ckpt_.put(key, CheckpointEntry{f.volume, f.index, f.committed_off});
        std::fprintf(stderr, "[follow] слежу: %s (со смещения %llu%s)\n", key.c_str(),
                     static_cast<unsigned long long>(resume),
                     f.gated ? ", ждёт >=100 байт" : "");
        files_.emplace(key, std::move(f));
    }

    // Открытие с ОБЯЗАТЕЛЬНЫМИ share-флагами; идентичность — с handle.
    bool open_handle(TailFile& f) {
        f.h = CreateFileW(f.path.c_str(), GENERIC_READ,
                          FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE, nullptr,
                          OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL | FILE_FLAG_SEQUENTIAL_SCAN,
                          nullptr);
        if (f.h == INVALID_HANDLE_VALUE) return false;
        BY_HANDLE_FILE_INFORMATION info{};
        if (!GetFileInformationByHandle(f.h, &info)) {
            CloseHandle(f.h);
            f.h = INVALID_HANDLE_VALUE;
            return false;
        }
        f.volume = info.dwVolumeSerialNumber;
        f.index = (static_cast<std::uint64_t>(info.nFileIndexHigh) << 32) | info.nFileIndexLow;
        return true;
    }

    // --- чтение и события ------------------------------------------------------
    // true — файл «горячий» (упёрлись в лимит чтения за тик): петля не спит.
    bool poll_file(TailFile& f) {
        if (f.h == INVALID_HANDLE_VALUE) return false;

        LARGE_INTEGER sz{};
        if (!GetFileSizeEx(f.h, &sz)) return false;
        const std::uint64_t hsize = static_cast<std::uint64_t>(sz.QuadPart);

        // Усечение (в т.ч. до 0): handle видит новый (меньший) размер.
        // Контракт: сброс на 0 и продолжаем работать. Открытое событие и
        // непрочитанный хвост усечённого содержимого отбрасываются (тех байт
        // больше не существует), committed тоже 0.
        if (hsize < f.read_off) {
            std::fprintf(stderr,
                         "[follow] усечение %s: %llu → %llu байт, смещения сброшены\n",
                         f.path_u8.c_str(), static_cast<unsigned long long>(f.read_off),
                         static_cast<unsigned long long>(hsize));
            f.read_off = f.parsed_off = f.committed_off = 0;
            f.raw_buf.clear();
            f.open_event.clear();
            f.last_data_ms = steady_ms();
            f.gated = true; // гейт MIN_FILE_SIZE переоценивается заново
            ckpt_.put(f.path_u8, CheckpointEntry{f.volume, f.index, 0});
            // rows_pending НЕ трогаем: строки до-усечённого содержимого уже в
            // батче; их ack выставит committed = parsed (уже новые значения).
        }

        // Гейт MIN_FILE_SIZE=100 — по размеру с handle (см. discover): пока
        // файл мал, не читаем; вырос — гейт снят навсегда (до сброса).
        if (f.gated) {
            if (hsize < tj::parse::kMinFileSize) return false;
            f.gated = false;
        }

        bool hot = false;
        std::uint64_t read_this_tick = 0;
        while (f.read_off < hsize) {
            if (read_this_tick >= kMaxReadPerTick) {
                hot = true; // дочитаем на следующем тике без сна
                break;
            }
            const std::uint64_t want64 = std::min<std::uint64_t>(
                {hsize - f.read_off, static_cast<std::uint64_t>(kReadChunk),
                 kMaxReadPerTick - read_this_tick});
            const DWORD want = static_cast<DWORD>(want64);
            read_buf_.resize(want);
            OVERLAPPED ov{};
            ov.Offset = static_cast<DWORD>(f.read_off & 0xFFFFFFFFu);
            ov.OffsetHigh = static_cast<DWORD>(f.read_off >> 32);
            DWORD got = 0;
            if (!ReadFile(f.h, read_buf_.data(), want, &got, &ov)) {
                const DWORD e = GetLastError();
                if (e != ERROR_HANDLE_EOF) {
                    std::fprintf(stderr, "Ошибка чтения %s: код WinAPI %lu\n",
                                 f.path_u8.c_str(), static_cast<unsigned long>(e));
                }
                break; // усечение между GetFileSizeEx и ReadFile поймает следующий тик
            }
            if (got == 0) break;
            f.raw_buf.append(read_buf_.data(), got);
            f.read_off += got;
            read_this_tick += got;
            bytes_read_ += got;
            f.last_data_ms = steady_ms();
            consume_lines(f); // нарезаем сразу — raw_buf держит только хвост
        }

        // Детекция подмены пути (ротация с пересозданием): путь указывает на
        // ДРУГОЙ файл, чем наш handle. Сравнивать размеры по пути и по handle
        // НЕЛЬЗЯ (метаданные каталога для открытого писателем файла лениво
        // отстают — вечные ложные срабатывания); вместо этого раз в
        // kReplaceProbeMs сверяем идентичность пробным открытием — только на
        // полностью дочитанном handle (старые данные уже дренированы).
        if (f.exists && f.read_off >= hsize) {
            const long long now = steady_ms();
            if (now - f.last_probe_ms >= kReplaceProbeMs) {
                f.last_probe_ms = now;
                hot = check_replaced(f) || hot;
            }
        }
        return hot;
    }

    // Путь и handle разошлись: если идентичность сменилась — файл пересоздан.
    // Старый handle дочитан (read_off == его EOF): дренируем \n-терминированное
    // открытое событие, принимаем новый handle со смещения 0.
    bool check_replaced(TailFile& f) {
        TailFile probe;
        probe.path = f.path;
        if (!open_handle(probe)) return false; // путь мог исчезнуть — не страшно
        if (probe.volume == f.volume && probe.index == f.index) {
            CloseHandle(probe.h); // тот же файл — просто гонка размеров
            return false;
        }
        std::fprintf(stderr, "[follow] файл пересоздан: %s — начинаю с нуля\n",
                     f.path_u8.c_str());
        if (!f.open_event.empty()) {
            emit_open_event(f, f.read_off - f.raw_buf.size()); // дренаж старого
        }
        CloseHandle(f.h);
        f.h = probe.h;
        f.volume = probe.volume;
        f.index = probe.index;
        f.read_off = f.parsed_off = f.committed_off = 0;
        f.raw_buf.clear();
        f.open_event.clear();
        f.last_data_ms = steady_ms();
        f.gated = true; // гейт нового файла переоценит следующий вызов poll_file
        ckpt_.put(f.path_u8, CheckpointEntry{f.volume, f.index, 0});
        return true; // новый файл может уже содержать данные — не спим
    }

    // Нарезка raw_buf на ЦЕЛЫЕ строки ('\n'-терминированные): строка-маска
    // закрывает предыдущее событие (правило 1) и открывает новое; прочие
    // строки — продолжение открытого события либо мусор до первой маски
    // (отбрасывается, как в batch split_events). BOM пропускается только на
    // смещении 0 (как в batch, KI-6). Незавершённый хвост остаётся в raw_buf.
    void consume_lines(TailFile& f) {
        std::size_t pos = 0;
        const std::uint64_t buf_base = f.read_off - f.raw_buf.size();
        while (pos < f.raw_buf.size()) {
            const void* nl =
                std::memchr(f.raw_buf.data() + pos, '\n', f.raw_buf.size() - pos);
            if (!nl) break;
            const std::size_t nl_idx =
                static_cast<std::size_t>(static_cast<const char*>(nl) - f.raw_buf.data());
            const char* line = f.raw_buf.data() + pos;
            std::size_t line_len = nl_idx - pos + 1; // включая '\n'
            const std::uint64_t line_abs = buf_base + pos;
            const std::uint64_t line_end_abs = line_abs + line_len; // сырой диапазон
            pos = nl_idx + 1;

            // BOM: только на смещении 0. Вырезается из СОДЕРЖИМОГО (маска/данные),
            // но остаётся в учёте смещений (line_end_abs выше уже включает его).
            if (line_abs == 0 && line_len >= 3 &&
                static_cast<unsigned char>(line[0]) == 0xEF &&
                static_cast<unsigned char>(line[1]) == 0xBB &&
                static_cast<unsigned char>(line[2]) == 0xBF) {
                line += 3;
                line_len -= 3;
            }

            if (tj::parse::is_event_start(line, line_len)) {
                if (!f.open_event.empty()) emit_open_event(f, line_abs); // правило (1)
                f.open_event.assign(line, line_len);
                f.parsed_off = line_abs; // начало диапазона события (BOM учтён позади)
            } else if (!f.open_event.empty()) {
                f.open_event.append(line, line_len);
                if (f.open_event.size() > kOpenEventGuard) {
                    // Страховка от «вечного события» (писатель без строк-масок):
                    // принудительное закрытие, иначе неограниченный рост RAM.
                    std::fprintf(stderr,
                                 "[follow] ВНИМАНИЕ: событие в %s превысило %zu МБ — "
                                 "принудительное закрытие\n",
                                 f.path_u8.c_str(), kOpenEventGuard >> 20);
                    emit_open_event(f, line_end_abs);
                }
            } else {
                // Мусор до первой маски: потреблён и никуда не эмитится.
                f.parsed_off = line_end_abs;
            }
        }
        f.raw_buf.erase(0, pos);
    }

    // Эмиссия открытого события: RowBinary → батч sink. parsed_off двигается
    // на end_abs ДО append: инвариант ack «committed = parsed покрывает ровно
    // отданные sink байты» держится, т.к. петля однопоточна. Ошибка вставки
    // уходит в bounded-ретраи с бэкоффом.
    void emit_open_event(TailFile& f, std::uint64_t end_abs) {
        f.parsed_off = end_abs;
        chunk_.clear();
        if (tj::parse::append_event_rowbinary(chunk_, f.open_event.data(),
                                              f.open_event.size(), f.rbctx)) {
            ++f.events;
            ++events_;
            ++f.rows_pending;
            f.open_event.clear();
            append_with_retry(chunk_.data(), chunk_.size(), 1);
            note_ack_if_any();
        } else {
            ++f.parse_skips;
            ++parse_skips_;
            f.open_event.clear();
        }
    }

    // Правило (2): хвост оканчивается '\n' (raw_buf пуст — все прочитанные
    // байты нарезаны) и данных не было idle_close_ms → эмитим открытое
    // событие. Незавершённая строка в raw_buf блокирует idle-закрытие: она
    // может оказаться продолжением этого события.
    void idle_close_pass() {
        const long long now = steady_ms();
        for (auto& kv : files_) {
            TailFile& f = kv.second;
            if (!f.open_event.empty() && f.raw_buf.empty() &&
                now - f.last_data_ms >= static_cast<long long>(cfg_.idle_close_ms)) {
                emit_open_event(f, f.read_off); // конец события = всё прочитанное
            }
        }
    }

    // --- sink: ретраи, ack, чекпоинты ---------------------------------------
    void append_with_retry(const char* data, std::size_t len, std::uint64_t rows) {
        try {
            sink_->append(data, len, rows); // пороги sink могут флашить внутри
            return;
        } catch (const std::exception& e) {
            std::fprintf(stderr, "[follow] сбой вставки: %s\n", e.what());
        }
        // Данные уже в батче sink (append копит до флаша) — добиваем ретраями.
        flush_with_retry();
    }

    void flush_if_due() {
        if (sink_->pending_rows() == 0) return;
        if (steady_ms() - last_ok_flush_ms_ >= static_cast<long long>(cfg_.ch.flush_ms)) {
            flush_with_retry();
        }
    }

    // Bounded retries с экспоненциальным бэкоффом 1..30 с. Пока идёт ретрай,
    // петля стоит — чтение файлов не продвигается (естественный бэкпрешер).
    // Исчерпание попыток — FatalSinkError (exit 1, чекпоинты по последнему ack).
    void flush_with_retry() {
        std::uint64_t backoff = kBackoffStartMs;
        for (int attempt = 1;; ++attempt) {
            try {
                sink_->flush_pending();
                note_ack_if_any();
                return;
            } catch (const std::exception& e) {
                if (attempt >= kMaxFlushRetries) throw FatalSinkError{e.what()};
                std::fprintf(stderr,
                             "[follow] ClickHouse недоступен (попытка %d/%d, пауза %llu мс): "
                             "%s\n",
                             attempt, kMaxFlushRetries,
                             static_cast<unsigned long long>(backoff), e.what());
                Sleep(static_cast<DWORD>(backoff));
                backoff = std::min<std::uint64_t>(backoff * 2, kBackoffCapMs);
            }
        }
    }

    // Ack = успешный HTTP 200 на батч (inserted_rows выросли). Всё, что было
    // отдано sink до этого момента, вставлено: committed каждого файла догоняет
    // parsed (петля однопоточна — во время POST ничего не парсится).
    // Чекпоинт персистится СРАЗУ после ack: окно дублей при kill сжимается с
    // периода тика до одного незавершённого флаша (замер: kill -Force на
    // 1333 соб/с давал 1200 дублей с отложенным персистом и 0–200 с немедленным).
    void note_ack_if_any() {
        const std::uint64_t ins = sink_->inserted_rows();
        if (ins == last_inserted_) return;
        last_inserted_ = ins;
        last_ok_flush_ms_ = steady_ms();
        for (auto& kv : files_) {
            TailFile& f = kv.second;
            f.rows_pending = 0;
            if (f.committed_off != f.parsed_off) {
                f.committed_off = f.parsed_off;
                ckpt_.put(f.path_u8, CheckpointEntry{f.volume, f.index, f.committed_off});
            }
        }
        if (ckpt_.dirty() && !ckpt_.save()) {
            std::fprintf(stderr,
                         "[follow] ВНИМАНИЕ: не удалось записать чекпоинты в %s (повторю)\n",
                         cfg_.state_dir.c_str());
        }
    }

    // Файл без строк в неподтверждённом батче коммитит прогресс сразу
    // (поток parse_skip-строк, мусор до первой маски): ack ему не нужен,
    // min-contiguous не нарушается — впереди его committed нет чужих строк.
    void advance_rowless_commits() {
        for (auto& kv : files_) {
            TailFile& f = kv.second;
            if (f.rows_pending == 0 && f.committed_off != f.parsed_off) {
                f.committed_off = f.parsed_off;
                ckpt_.put(f.path_u8, CheckpointEntry{f.volume, f.index, f.committed_off});
            }
        }
    }

    void maybe_save_checkpoints(bool force) {
        const long long now = steady_ms();
        if (!force && now - last_ckpt_save_ms_ < static_cast<long long>(kCheckpointSaveMs)) {
            return;
        }
        last_ckpt_save_ms_ = now;
        if (ckpt_.dirty() && !ckpt_.save()) {
            std::fprintf(stderr,
                         "[follow] ВНИМАНИЕ: не удалось записать чекпоинты в %s (повторю)\n",
                         cfg_.state_dir.c_str());
        }
    }

    // --- удалённые файлы -------------------------------------------------------
    // Путь исчез из дискавери (полного!) и handle дочитан: дренируем
    // \n-терминированное открытое событие (данных больше не будет), закрываем,
    // снимаем слежение. Чекпоинт остаётся: пересоздание пути даст новую
    // идентичность → старт с нуля.
    void reap_gone_files() {
        if (!last_walk_complete_) return;
        for (auto it = files_.begin(); it != files_.end();) {
            TailFile& f = it->second;
            bool reap = false;
            if (!f.exists && f.h != INVALID_HANDLE_VALUE) {
                LARGE_INTEGER sz{};
                if (f.gated) {
                    reap = true; // не читался вовсе — дренировать нечего
                } else if (GetFileSizeEx(f.h, &sz) &&
                           f.read_off >= static_cast<std::uint64_t>(sz.QuadPart)) {
                    reap = true;
                }
            }
            if (reap) {
                if (!f.open_event.empty()) {
                    emit_open_event(f, f.read_off - f.raw_buf.size());
                }
                CloseHandle(f.h);
                f.h = INVALID_HANDLE_VALUE;
                std::fprintf(stderr, "[follow] файл удалён, слежение снято: %s\n",
                             f.path_u8.c_str());
                it = files_.erase(it);
            } else {
                ++it;
            }
        }
    }

    // --- стоп/дренаж -----------------------------------------------------------
    // Правило (3): дочитать всё до EOF, эмитить только \n-терминированный
    // остаток открытых событий; незавершённая строка (raw_buf без '\n') не
    // эмитится никогда — committed останавливается ПЕРЕД ней, рестарт дочитает.
    void drain_and_stop() {
        std::fprintf(stderr, "[follow] стоп-файл обнаружен — дренаж\n");
        discover(); // подобрать файлы, созданные в последний момент
        bool more = true;
        while (more) { // дочитываем без лимита тика
            more = false;
            for (auto& kv : files_) more = poll_file(kv.second) || more;
        }
        for (auto& kv : files_) {
            TailFile& f = kv.second;
            if (!f.open_event.empty()) {
                emit_open_event(f, f.read_off - f.raw_buf.size());
            }
        }
        flush_with_retry(); // финальный флаш (пустой батч — no-op)
        advance_rowless_commits();
    }

    // --- телеметрия --------------------------------------------------------------
    void maybe_progress() {
        const long long now = steady_ms();
        if (now - last_progress_ms_ < static_cast<long long>(kProgressMs)) return;
        last_progress_ms_ = now;
        progress_line("работаю");
    }

    void progress_line(const char* tag) {
        std::fprintf(stderr,
                     "[follow] %s: файлов=%zu событий=%" PRIu64 " вставлено=%" PRIu64
                     " в_батче=%" PRIu64 " parse_skips=%" PRIu64 " прочитано=%.1f МБ\n",
                     tag, files_.size(), events_,
                     sink_ ? sink_->inserted_rows() : 0,
                     sink_ ? sink_->pending_rows() : 0, parse_skips_,
                     static_cast<double>(bytes_read_) / (1024.0 * 1024.0));
        std::fflush(stderr);
    }

    // Схема --stats-json — как у batch (+inserted_rows): приёмник обязан
    // игнорировать незнакомые поля, порядок ключей алфавитный.
    // small_file_skips — файлы, так и не прошедшие гейт MIN_FILE_SIZE к выходу.
    void write_stats() {
        if (cfg_.stats_json.empty()) return;
        FILE* out = _wfopen(fs::u8path(cfg_.stats_json).wstring().c_str(), L"wb");
        if (!out) {
            std::fprintf(stderr, "Ошибка записи --stats-json %s\n", cfg_.stats_json.c_str());
            return;
        }
        std::uint64_t gated = 0;
        for (const auto& kv : files_) {
            if (kv.second.gated) ++gated;
        }
        std::fprintf(out,
                     "{\"bytes\":%" PRIu64 ",\"events\":%" PRIu64 ",\"failed_files\":%" PRIu64
                     ",\"files\":%" PRIu64 ",\"inserted_rows\":%" PRIu64
                     ",\"parse_skips\":%" PRIu64 ",\"skips\":%" PRIu64
                     ",\"small_file_skips\":%" PRIu64 "}\n",
                     bytes_read_, events_, failed_files_,
                     static_cast<std::uint64_t>(files_.size()) - gated,
                     sink_ ? sink_->inserted_rows() : 0, parse_skips_,
                     parse_skips_ + gated, gated);
        if (std::fclose(out) != 0) {
            std::fprintf(stderr, "Ошибка записи --stats-json %s\n", cfg_.stats_json.c_str());
        }
    }

    FollowConfig cfg_;
    fs::path input_abs_;
    std::wstring stop_path_w_;
    CheckpointStore ckpt_;
    std::unique_ptr<ClickHouseSink> sink_;
    std::map<std::string, TailFile> files_;
    std::set<std::string> walk_errors_seen_;
    std::set<std::string> open_errors_seen_;
    std::vector<char> read_buf_;
    std::string chunk_; // scratch: RowBinary одного события
    bool last_walk_complete_ = false;

    std::uint64_t last_inserted_ = 0;
    long long last_ok_flush_ms_ = 0;
    long long last_progress_ms_ = 0;
    long long last_ckpt_save_ms_ = 0;

    std::uint64_t events_ = 0;
    std::uint64_t parse_skips_ = 0;
    std::uint64_t bytes_read_ = 0;
    std::uint64_t failed_files_ = 0;
};

} // namespace

int run_follow(const FollowConfig& cfg) {
    Follower fl(cfg);
    return fl.run();
}

#else // !_WIN32

int run_follow(const FollowConfig&) {
    std::fprintf(stderr, "--follow поддерживается только в Windows-сборке\n");
    return 1;
}

#endif

} // namespace tj_cli
