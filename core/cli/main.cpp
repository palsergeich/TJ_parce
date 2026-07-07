// tj-agent-cpp — участник bake-off (C++): тонкий CLI поверх библиотеки tj_core.
//
// Два синтаксиса запуска (оба обязательны):
//
//  1. Контракт golden-раннера (совместим с cpp_parse/count_contexts.exe):
//     tj-agent-cpp <input_dir> [workers] [output.jsonl] [--no-output]
//
//  2. Контракт bake-off (docs/bakeoff-protocol.md §1.1, batch-режим):
//     tj-agent-cpp --input <dir> [--threads N]
//                  [--sink null|file:<path>|clickhouse[:<url>]]
//                  [--batch-rows N] [--batch-bytes N] [--flush-ms N]
//                  [--stats-json <path>]
//
// --sink clickhouse: события кодируются ядром в RowBinary (без пересборки
// JSON) и вставляются по HTTP (INSERT ... FORMAT RowBinary, WinHTTP, без
// внешних зависимостей). URL: http://host[:port][/<db>.<table>], по умолчанию
// http://localhost:8123/tj_bench.events. Батч: 50000 строк | 64 МБ | 1000 мс.
// --stats-json дополняется полем inserted_rows.
//
// Формат вывода — docs/format-spec.md v1.0 rev 3: NDJSON без BOM, LF-терминатор
// каждой записи. Порядок записей внутри файла = порядок событий в файле при
// любом числе потоков (жёстче KI-11); файлы — по убыванию размера.
//
// Данные пишутся ТОЛЬКО в файл (никогда в stdout). stdout — прогресс/сводка
// в файловом режиме (обычный UTF-8 через FILE*, без WriteConsoleW — редирект
// в файл/пайп ничего не теряет). stderr — только реальные ошибки: golden-раннер
// на PowerShell 5.1 с $ErrorActionPreference='Stop' трактует stderr-строки
// нативного процесса под редиректом как ошибки.
//
// Exit-коды: 0 — успех; 1 — ошибка аргументов/каталога/записи вывода;
// 2 — часть входных файлов не удалось прочитать (KI-12).
#include <tj/normalizer.hpp>

#include <chrono>
#include <cinttypes>
#include <climits>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <exception>
#include <filesystem>
#include <memory>
#include <string>
#include <system_error>
#include <thread>
#include <vector>

#include "clickhouse_sink.hpp"

#ifdef _WIN32
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#endif

namespace fs = std::filesystem;

namespace {

struct CliConfig {
    std::string input;
    unsigned workers = 0;   // 0 → hardware_concurrency (клампится в parse_args)
    std::string output;     // путь к NDJSON; пуст при null_sink/clickhouse
    bool null_sink = false;
    bool clickhouse = false;
    std::string ch_dsn;     // хвост "--sink clickhouse:<dsn>" (пуст → дефолты)
    std::uint64_t batch_rows = 50000;          // батч ClickHouse: строк
    std::uint64_t batch_bytes = 64ull << 20;   // … или байт
    std::uint64_t flush_ms = 1000;             // … или мс с прошлого флаша
    std::string stats_json;
};

void usage() {
    std::fprintf(stderr,
                 "Использование:\n"
                 "  tj-agent-cpp <input_dir> [workers] [output.jsonl] [--no-output]\n"
                 "  tj-agent-cpp --input <dir> [--threads N] "
                 "[--sink null|file:<path>|clickhouse[:<url>]]\n"
                 "               [--batch-rows N] [--batch-bytes N] [--flush-ms N] "
                 "[--stats-json <path>]\n");
}

bool parse_u64(const char* s, std::uint64_t& out, const char* what) {
    char* end = nullptr;
    unsigned long long v = std::strtoull(s, &end, 10);
    if (end == s || *end != '\0' || v == 0ull || v == ULLONG_MAX) {
        std::fprintf(stderr, "Ошибка: %s должен быть положительным целым числом\n", what);
        return false;
    }
    out = static_cast<std::uint64_t>(v);
    return true;
}

unsigned default_workers() {
    unsigned n = std::thread::hardware_concurrency();
    if (n < 1) n = 1;
    if (n > 1024) n = 1024;
    return n;
}

bool parse_workers(const char* s, unsigned& out, const char* what) {
    char* end = nullptr;
    long w = std::strtol(s, &end, 10);
    if (end == s || *end != '\0' || w < 1 || w > 1024) {
        std::fprintf(stderr, "Ошибка: %s должен быть целым числом от 1 до 1024\n", what);
        return false;
    }
    out = static_cast<unsigned>(w);
    return true;
}

// Флаговый контракт bake-off (семантика Go-агента, agents/go).
bool parse_flag_args(int argc, char** argv, CliConfig& cfg) {
    std::string sink;
    auto next = [&](int i) -> const char* {
        if (i + 1 >= argc) {
            std::fprintf(stderr, "Ошибка: у флага %s нет значения\n", argv[i]);
            return nullptr;
        }
        return argv[i + 1];
    };
    for (int i = 1; i < argc; ++i) {
        const char* a = argv[i];
        if (std::strcmp(a, "--input") == 0) {
            const char* v = next(i);
            if (!v) return false;
            cfg.input = v;
            ++i;
        } else if (std::strcmp(a, "--threads") == 0) {
            const char* v = next(i);
            if (!v) return false;
            if (!parse_workers(v, cfg.workers, "--threads")) return false;
            ++i;
        } else if (std::strcmp(a, "--sink") == 0) {
            const char* v = next(i);
            if (!v) return false;
            sink = v;
            ++i;
        } else if (std::strcmp(a, "--stats-json") == 0) {
            const char* v = next(i);
            if (!v) return false;
            cfg.stats_json = v;
            ++i;
        } else if (std::strcmp(a, "--batch-rows") == 0) {
            const char* v = next(i);
            if (!v || !parse_u64(v, cfg.batch_rows, "--batch-rows")) return false;
            ++i;
        } else if (std::strcmp(a, "--batch-bytes") == 0) {
            const char* v = next(i);
            if (!v || !parse_u64(v, cfg.batch_bytes, "--batch-bytes")) return false;
            ++i;
        } else if (std::strcmp(a, "--flush-ms") == 0) {
            // Параметры батчирования влияют только на clickhouse-sink;
            // для file/null принимаются и игнорируются (контракт bake-off).
            const char* v = next(i);
            if (!v || !parse_u64(v, cfg.flush_ms, "--flush-ms")) return false;
            ++i;
        } else if (std::strcmp(a, "--follow") == 0) {
            std::fprintf(stderr, "Ошибка: --follow пока не реализован (фаза 3)\n");
            return false;
        } else {
            std::fprintf(stderr, "Ошибка: неизвестный флаг %s\n", a);
            usage();
            return false;
        }
    }
    if (cfg.input.empty()) {
        std::fprintf(stderr, "Ошибка: обязателен --input <dir>\n");
        return false;
    }
    if (sink.empty()) {
        std::fprintf(stderr,
                     "Ошибка: обязателен --sink {null|file:<path>|clickhouse[:<url>]}\n");
        return false;
    }
    if (sink == "null") {
        cfg.null_sink = true;
    } else if (sink.rfind("file:", 0) == 0) {
        cfg.output = sink.substr(5);
        if (cfg.output.empty()) {
            std::fprintf(stderr, "Ошибка: пустой путь в --sink file:<path>\n");
            return false;
        }
    } else if (sink == "clickhouse") {
        cfg.clickhouse = true;
    } else if (sink.rfind("clickhouse:", 0) == 0) {
        cfg.clickhouse = true;
        cfg.ch_dsn = sink.substr(11);
        if (cfg.ch_dsn.empty()) {
            std::fprintf(stderr, "Ошибка: пустой DSN в --sink clickhouse:<url>\n");
            return false;
        }
    } else {
        std::fprintf(stderr, "Ошибка: неизвестный sink \"%s\"\n", sink.c_str());
        return false;
    }
    return true;
}

bool parse_args(int argc, char** argv, CliConfig& cfg) {
    cfg.workers = default_workers();
    if (argc < 2) {
        usage();
        return false;
    }
    if (std::strncmp(argv[1], "--", 2) == 0) return parse_flag_args(argc, argv, cfg);

    // Позиционный контракт golden-раннера
    cfg.input = argv[1];
    if (argc >= 3) {
        // KI-8: строгая валидация (atoi превращал "-2" в ~1.8e19 потоков)
        if (!parse_workers(argv[2], cfg.workers, "workers")) return false;
    }
    if (argc >= 4) {
        cfg.output = argv[3];
    } else {
        std::error_code ec;
        fs::path cwd = fs::current_path(ec);
        if (ec) {
            std::fprintf(stderr, "Ошибка определения текущей директории: %s\n",
                         ec.message().c_str());
            return false;
        }
        cfg.output = (cwd / "result.jsonl").u8string();
    }
    if (argc >= 5) {
        const char* flag = argv[4];
        if (std::strcmp(flag, "--no-output") == 0 || std::strcmp(flag, "--no-write") == 0 ||
            std::strcmp(flag, "--dry-run") == 0) {
            cfg.null_sink = true;
            cfg.output.clear();
        }
    }
    return true;
}

// Контракт bakeoff-protocol §3: {"events":N,"files":M,"skips":K,"bytes":B}
// плюс расшифровка skips отдельными полями (приёмник обязан игнорировать
// незнакомые). Порядок ключей — как у Go-агента (алфавитный).
// inserted_rows — только для clickhouse-sink (nullptr → поле не пишется).
void write_stats_json(const CliConfig& cfg, const tj::RunStats& st,
                      const std::uint64_t* inserted_rows = nullptr) {
    if (cfg.stats_json.empty()) return;
#ifdef _WIN32
    FILE* f = _wfopen(fs::u8path(cfg.stats_json).wstring().c_str(), L"wb");
#else
    FILE* f = std::fopen(cfg.stats_json.c_str(), "wb");
#endif
    if (!f) {
        std::fprintf(stderr, "Ошибка записи --stats-json %s\n", cfg.stats_json.c_str());
        return;
    }
    std::fprintf(f, "{\"bytes\":%" PRIu64 ",\"events\":%" PRIu64 ",\"failed_files\":%" PRIu64
                 ",\"files\":%" PRIu64, st.bytes, st.events, st.failed_files, st.files);
    if (inserted_rows) {
        std::fprintf(f, ",\"inserted_rows\":%" PRIu64, *inserted_rows);
    }
    std::fprintf(f,
                 ",\"parse_skips\":%" PRIu64 ",\"skips\":%" PRIu64
                 ",\"small_file_skips\":%" PRIu64 "}\n",
                 st.parse_skips, st.parse_skips + st.small_file_skips, st.small_file_skips);
    if (std::fclose(f) != 0) {
        std::fprintf(stderr, "Ошибка записи --stats-json %s\n", cfg.stats_json.c_str());
    }
}

// --sink clickhouse: писатель конвейера отдаёт RowBinary-чанки прямиком в
// HTTP-батчер. Ошибка вставки/подключения фатальна (exit 1) — конвейер при
// этом корректно дренируется (см. NormalizerPipeline::run_rowbinary).
int run_clickhouse(const CliConfig& cfg, tj::NormalizerPipeline& pipeline,
                   const tj::NormalizerPipeline::FileFn& on_file) {
    tj_cli::ClickHouseConfig chc;
    chc.batch_rows = cfg.batch_rows;
    chc.batch_bytes = cfg.batch_bytes;
    chc.flush_ms = cfg.flush_ms;
    std::string dsn_err;
    if (!tj_cli::parse_clickhouse_dsn(cfg.ch_dsn, chc, dsn_err)) {
        std::fprintf(stderr, "Ошибка: %s\n", dsn_err.c_str());
        return 1;
    }

    auto start = std::chrono::steady_clock::now();

    std::unique_ptr<tj_cli::ClickHouseSink> sink;
    try {
        sink = std::make_unique<tj_cli::ClickHouseSink>(std::move(chc));
    } catch (const std::exception& e) {
        std::fprintf(stderr, "Ошибка подключения к ClickHouse: %s\n", e.what());
        return 1;
    }

    tj::RunStats st;
    try {
        st = pipeline.run_rowbinary(
            [&](const char* data, std::size_t len, std::uint64_t rows) {
                sink->append(data, len, rows);
            },
            on_file);
        sink->finish();
    } catch (const std::exception& e) {
        std::fprintf(stderr, "Ошибка вставки в ClickHouse: %s\n", e.what());
        return 1;
    }

    double sec = std::chrono::duration<double>(std::chrono::steady_clock::now() - start).count();
    double mb = static_cast<double>(st.bytes) / (1024.0 * 1024.0);
    double speed = sec > 0 ? mb / sec : 0.0;
    std::uint64_t inserted = sink->inserted_rows();
    std::fprintf(stdout,
                 "Файлов: %" PRIu64 " (ошибок открытия: %" PRIu64 ", пропущено <100 байт: %" PRIu64
                 ") | Событий: %" PRIu64 " | parse_skips: %" PRIu64
                 " | %.2f МБ за %.3f с (%.1f МБ/с, workers=%u)\n",
                 st.files, st.failed_files, st.small_file_skips, st.events, st.parse_skips,
                 mb, sec, speed, cfg.workers);
    std::fprintf(stdout, "ClickHouse: вставлено %" PRIu64 " строк за %.3f с (%.0f строк/с)\n",
                 inserted, sec, sec > 0 ? static_cast<double>(inserted) / sec : 0.0);

    write_stats_json(cfg, st, &inserted);

    if (st.failed_files > 0) {
        std::fprintf(stderr, "ВНИМАНИЕ: часть файлов не обработана (см. счётчик ошибок)\n");
        return 2;
    }
    return 0;
}

int run(int argc, char** argv) {
    CliConfig cfg;
    if (!parse_args(argc, argv, cfg)) return 1;

    fs::path input = fs::u8path(cfg.input);
    std::error_code ec;
    if (!fs::exists(input, ec) || ec) {
        std::fprintf(stderr, "Ошибка: директория не существует: %s\n", cfg.input.c_str());
        return 1;
    }
    if (!fs::is_directory(input, ec) || ec) {
        std::fprintf(stderr, "Ошибка: указанный путь не является директорией: %s\n",
                     cfg.input.c_str());
        return 1;
    }

    tj::Config pcfg;
    pcfg.workers = cfg.workers;
    pcfg.on_error = [](const std::string& msg) { std::fprintf(stderr, "%s\n", msg.c_str()); };
    tj::NormalizerPipeline pipeline(pcfg);
    pipeline.add_dir(input);

    if (pipeline.file_count() == 0) {
        // Выходной файл НЕ создаётся, exit 0 (format-spec §6; golden-раннер
        // трактует отсутствующий файл как вывод нулевой длины).
        std::fprintf(stdout, "Не найдено .log файлов для обработки\n");
        tj::RunStats st = pipeline.run(nullptr); // только счётчики discovery
        const std::uint64_t zero_rows = 0;
        write_stats_json(cfg, st, cfg.clickhouse ? &zero_rows : nullptr);
        return static_cast<int>(st.failed_files > 0 ? 2 : 0);
    }

    // Прогресс — в stdout, как у эталона (раз в 50 файлов).
    std::uint64_t files_done = 0;
    const std::uint64_t files_total = pipeline.file_count();
    tj::NormalizerPipeline::FileFn on_file = [&](const tj::FileCompletion&) {
        ++files_done;
        if (files_done % 50 == 0 || files_done == files_total) {
            std::fprintf(stdout, "Прочитано: %" PRIu64 "/%" PRIu64 " файлов (%.1f%%)\n",
                         files_done, files_total,
                         100.0 * static_cast<double>(files_done) /
                             static_cast<double>(files_total));
            std::fflush(stdout);
        }
    };

    if (cfg.clickhouse) return run_clickhouse(cfg, pipeline, on_file);

    auto start = std::chrono::steady_clock::now();

    // Выход открываем до разбора: пустой (но существующий) файл — валидный
    // результат, если все события отфильтрованы (как у эталонного exe).
    FILE* out = nullptr;
    if (!cfg.null_sink) {
        fs::path out_path = fs::u8path(cfg.output);
        if (out_path.has_parent_path() && !out_path.parent_path().empty()) {
            fs::create_directories(out_path.parent_path(), ec);
            if (ec) {
                std::fprintf(stderr, "Ошибка: не удалось создать директории для файла %s: %s\n",
                             cfg.output.c_str(), ec.message().c_str());
                return 1;
            }
        }
#ifdef _WIN32
        out = _wfopen(out_path.wstring().c_str(), L"wb");
#else
        out = std::fopen(cfg.output.c_str(), "wb");
#endif
        if (!out) {
            // KI-5: фатальная ошибка с ненулевым exit-кодом, не дедлок
            std::fprintf(stderr, "Ошибка: не удалось открыть файл для записи: %s\n",
                         cfg.output.c_str());
            return 1;
        }
        // KI-7: BOM в выходной NDJSON не пишем
    }

    // Ручной буфер записи 32 МБ (как у эталона): данные → только в файл.
    bool writer_failed = false;
    std::string wbuf;
    const std::size_t kWbufLimit = 32u << 20;
    if (out) wbuf.reserve(kWbufLimit + (1u << 20));
    auto flush_wbuf = [&]() {
        if (!out || wbuf.empty()) return;
        if (!writer_failed &&
            std::fwrite(wbuf.data(), 1, wbuf.size(), out) != wbuf.size()) {
            std::fprintf(stderr, "Ошибка записи в файл (диск полон?): %s\n", cfg.output.c_str());
            writer_failed = true; // дальше только дренируем (KI-5: без зависаний)
        }
        wbuf.clear();
    };

    tj::NormalizerPipeline::RecordFn on_record;
    if (out) {
        on_record = [&](const char* data, std::size_t len) {
            wbuf.append(data, len);
            wbuf.push_back('\n');
            if (wbuf.size() >= kWbufLimit) flush_wbuf();
        };
    }

    tj::RunStats st = pipeline.run(on_record, on_file);

    if (out) {
        flush_wbuf();
        if (std::fclose(out) != 0 && !writer_failed) {
            std::fprintf(stderr, "Ошибка закрытия файла %s\n", cfg.output.c_str());
            writer_failed = true;
        }
        out = nullptr;
    }

    double sec = std::chrono::duration<double>(std::chrono::steady_clock::now() - start).count();
    double mb = static_cast<double>(st.bytes) / (1024.0 * 1024.0);
    double speed = sec > 0 ? mb / sec : 0.0;
    // Успешная сводка — в stdout (stderr только для реальных ошибок).
    std::fprintf(stdout,
                 "Файлов: %" PRIu64 " (ошибок открытия: %" PRIu64 ", пропущено <100 байт: %" PRIu64
                 ") | Событий: %" PRIu64 " | parse_skips: %" PRIu64
                 " | %.2f МБ за %.3f с (%.1f МБ/с, workers=%u)\n",
                 st.files, st.failed_files, st.small_file_skips, st.events, st.parse_skips,
                 mb, sec, speed, cfg.workers);

    write_stats_json(cfg, st);

    if (writer_failed) {
        std::fprintf(stderr, "ОШИБКА: запись результатов не удалась, вывод неполный\n");
        return 1;
    }
    if (st.failed_files > 0) {
        std::fprintf(stderr, "ВНИМАНИЕ: часть файлов не обработана (см. счётчик ошибок)\n");
        return 2;
    }
    return 0;
}

} // namespace

#ifdef _WIN32
// Аргументы берём широкими (wmain) и конвертируем в UTF-8: обычный main()
// получил бы argv в ACP-кодировке, и кириллические пути (репозиторий
// «ТехЖурнал») ломались бы на не-русской локали. Дальше весь код работает
// только с UTF-8 (fs::u8path).
int wmain(int argc, wchar_t** wargv) {
    // UTF-8 в консоли Windows: данные в stdout не идут, поэтому обычного
    // перевода кодовой страницы достаточно (без WriteConsoleW — KI-13-safe).
    SetConsoleOutputCP(65001);
    SetConsoleCP(65001);
    try {
        std::vector<std::string> args;
        args.reserve(static_cast<std::size_t>(argc));
        for (int i = 0; i < argc; ++i) {
            int need = WideCharToMultiByte(CP_UTF8, 0, wargv[i], -1, nullptr, 0, nullptr, nullptr);
            std::vector<char> tmp(need > 0 ? static_cast<std::size_t>(need) : 1, '\0');
            if (need > 0) {
                WideCharToMultiByte(CP_UTF8, 0, wargv[i], -1, tmp.data(), need, nullptr, nullptr);
            }
            args.emplace_back(tmp.data()); // без завершающего NUL
        }
        std::vector<char*> ptrs;
        ptrs.reserve(args.size() + 1);
        for (std::string& s : args) ptrs.push_back(&s[0]);
        ptrs.push_back(nullptr);
        return run(argc, ptrs.data());
    } catch (const std::exception& e) {
        std::fprintf(stderr, "Критическая ошибка: %s\n", e.what());
        return 1;
    } catch (...) {
        std::fprintf(stderr, "Неизвестная критическая ошибка!\n");
        return 1;
    }
}
#else
int main(int argc, char** argv) {
    try {
        return run(argc, argv);
    } catch (const std::exception& e) {
        std::fprintf(stderr, "Критическая ошибка: %s\n", e.what());
        return 1;
    } catch (...) {
        std::fprintf(stderr, "Неизвестная критическая ошибка!\n");
        return 1;
    }
}
#endif
