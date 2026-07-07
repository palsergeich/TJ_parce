// pipeline.cpp — NormalizerPipeline: оконный mmap входа, БАЙТОВЫЙ бюджет
// допуска и упорядоченная выдача записей (детерминизм при любом workers —
// v1.1 §5).
//
// Модель памяти — байтовый бюджет допуска (выровнена с agents/go и
// agents/rust; прежнее окно «2×workers файлов» буферизовало целиком вывод
// любого неголовного файла — на стрессе 6+2 ГБ это давало peak RSS 2.4 ГБ
// против 52/71 МБ у Go/Rust):
//   - файлы сортируются по убыванию размера (stable — при равенстве порядок
//     обхода); воркеры берут их строго по возрастанию индекса;
//   - ГОЛОВНОЙ файл (i == files_written — его очередь писаться) допускается
//     БЕЗУСЛОВНО и СТРИМИТСЯ: писатель забирает чанки по мере готовности,
//     очередь чанков головы ограничена kHeadMaxChunks — вывод головы не
//     копится даже при медленном приёмнике;
//   - НЕголовной файл допускается, только если его размер помещается в
//     остаток бюджета max(64 МБ × workers, 256 МБ); размер списывается при
//     допуске и возвращается писателем после выдачи файла — на ЛЮБОМ пути,
//     включая ошибки разбора (done ставится всегда) и ошибки приёмника
//     (писатель продолжает дренировать все файлы);
//   - файл крупнее остатка бюджета ждёт, пока сам станет головой, и тогда
//     стримится без буферизации.
//
// Дедлок невозможен: допуск головы НИКОГДА не блокируется на бюджете (когда
// писатель ждёт непринятый головной файл, все предыдущие уже выданы — любой
// освободившийся воркер берёт голову безусловно); продюсер головы ждёт только
// при непустой очереди чанков — писатель гарантированно разгружает её и будит
// продюсера (cv_head).
#include <tj/normalizer.hpp>

#include <algorithm>
#include <atomic>
#include <condition_variable>
#include <cstring>
#include <exception>
#include <mutex>
#include <system_error>
#include <thread>
#include <vector>

#include "parser.hpp"
#include "util.hpp"

#ifdef _WIN32
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#else
#include <fcntl.h>     // KI-9: отсутствовал в cpp_parse — POSIX-сборка не компилировалась
#include <sys/mman.h>
#include <sys/stat.h>  // KI-9
#include <unistd.h>
#endif

namespace fs = std::filesystem;

namespace tj {

namespace {

// Гранулярность выравнивания смещения отображения: 64 КБ покрывает и
// allocation granularity Windows, и страницу POSIX.
constexpr std::uint64_t kMapGranularity = 64u << 10;
// Гвардейская зона у конца окна: строка-кандидат маски события должна
// целиком помещаться в окно, иначе окно переезжает. 64 КБ — с запасом
// (маске нужно ~15 байт + цифры длительности).
constexpr std::uint64_t kMapGuard = 64u << 10;

// Байтовый бюджет допуска неголовных файлов (модель agents/go, agents/rust).
constexpr std::uint64_t kAdmissionBytesPerWorker = 64ull << 20;
constexpr std::uint64_t kAdmissionBytesFloor = 256ull << 20;
// Ограничение очереди чанков ГОЛОВНОГО файла (стриминг): при медленном
// приёмнике продюсер головы притормаживает, а не копит вывод (16 × 4 МБ).
constexpr std::size_t kHeadMaxChunks = 16;

// Файл с оконным (скользящим) memory-mapping: вместо отображения целиком
// (наследие cpp_parse — 5.7-ГБ файл давал 5.7 ГБ WorkingSet) отображаются
// окна ограниченного размера, окно всегда покрывает текущее событие.
// POSIX-ветка компилируется (KI-9: добавлены <fcntl.h>, <sys/stat.h>).
class MappedWindowFile {
public:
    MappedWindowFile() = default;
    ~MappedWindowFile() { close(); }
    MappedWindowFile(const MappedWindowFile&) = delete;
    MappedWindowFile& operator=(const MappedWindowFile&) = delete;

    bool open(const fs::path& path) {
#ifdef _WIN32
        file_handle_ = CreateFileW(path.c_str(), GENERIC_READ, FILE_SHARE_READ, nullptr,
                                   OPEN_EXISTING,
                                   FILE_ATTRIBUTE_NORMAL | FILE_FLAG_SEQUENTIAL_SCAN, nullptr);
        if (file_handle_ == INVALID_HANDLE_VALUE) return false;

        LARGE_INTEGER file_size;
        if (!GetFileSizeEx(file_handle_, &file_size)) {
            close();
            return false;
        }
        size_ = static_cast<std::uint64_t>(file_size.QuadPart);
        if (size_ == 0) return true;

        mapping_handle_ = CreateFileMappingW(file_handle_, nullptr, PAGE_READONLY, 0, 0, nullptr);
        if (mapping_handle_ == nullptr) {
            close();
            return false;
        }
#else
        fd_ = ::open(path.c_str(), O_RDONLY);
        if (fd_ == -1) return false;
        struct stat st;
        if (fstat(fd_, &st) == -1) {
            close();
            return false;
        }
        size_ = static_cast<std::uint64_t>(st.st_size);
#endif
        return true;
    }

    // Отображает окно [base, base+len); base обязан быть кратен kMapGranularity,
    // base+len ≤ size(). Старое окно освобождается. nullptr при ошибке.
    const char* map_window(std::uint64_t base, std::uint64_t len) {
        unmap();
        if (len == 0) return nullptr;
#ifdef _WIN32
        view_ = static_cast<const char*>(
            MapViewOfFile(mapping_handle_, FILE_MAP_READ,
                          static_cast<DWORD>(base >> 32),
                          static_cast<DWORD>(base & 0xFFFFFFFFu),
                          static_cast<SIZE_T>(len)));
        if (!view_) return nullptr;
#else
        void* m = mmap(nullptr, static_cast<std::size_t>(len), PROT_READ, MAP_PRIVATE, fd_,
                       static_cast<off_t>(base));
        if (m == MAP_FAILED) return nullptr;
        view_ = static_cast<const char*>(m);
        madvise(const_cast<char*>(view_), static_cast<std::size_t>(len), MADV_SEQUENTIAL);
#endif
        view_len_ = len;
        return view_;
    }

    void close() {
        unmap();
#ifdef _WIN32
        if (mapping_handle_) CloseHandle(mapping_handle_);
        if (file_handle_ != INVALID_HANDLE_VALUE) CloseHandle(file_handle_);
        mapping_handle_ = nullptr;
        file_handle_ = INVALID_HANDLE_VALUE;
#else
        if (fd_ != -1) ::close(fd_);
        fd_ = -1;
#endif
        size_ = 0;
    }

    std::uint64_t size() const { return size_; }

private:
    void unmap() {
        if (view_) {
#ifdef _WIN32
            UnmapViewOfFile(view_);
#else
            munmap(const_cast<char*>(view_), static_cast<std::size_t>(view_len_));
#endif
            view_ = nullptr;
            view_len_ = 0;
        }
    }

    const char* view_ = nullptr;
    std::uint64_t view_len_ = 0;
    std::uint64_t size_ = 0;
#ifdef _WIN32
    HANDLE file_handle_ = INVALID_HANDLE_VALUE;
    HANDLE mapping_handle_ = nullptr;
#else
    int fd_ = -1;
#endif
};

// Скользящее сканирование файла окнами map_bytes: разрезание на события по
// маске (семантика parse::split_events, включая пропуск BOM — KI-6), но с
// ограниченной резидентностью. Окно всегда содержит текущее событие целиком
// [ev_start, scan], поэтому emit получает непрерывные байты.
// true — файл дочитан; false — ошибка отображения посреди файла.
template <class Emit>
bool scan_file_windowed(MappedWindowFile& f, std::uint64_t map_bytes, Emit&& emit) {
    const std::uint64_t fsize = f.size();
    if (fsize == 0) return true;

    std::uint64_t win_base = 0, win_len = 0;
    const char* win = nullptr;
    // Переотображает окно так, чтобы оно покрывало [need_start, want_end).
    auto remap = [&](std::uint64_t need_start, std::uint64_t want_end) -> bool {
        std::uint64_t base = need_start & ~(kMapGranularity - 1);
        std::uint64_t end = want_end < fsize ? want_end : fsize;
        win = f.map_window(base, end - base);
        if (!win) return false;
        win_base = base;
        win_len = end - base;
        return true;
    };

    if (!remap(0, map_bytes)) return false;

    std::uint64_t scan = 0;     // абсолютная позиция сканирования
    std::uint64_t ev_start = 0; // абсолютное начало текущего события
    // BOM в начале файла пропускается (KI-6)
    if (win_len >= 3 && static_cast<unsigned char>(win[0]) == 0xEF &&
        static_cast<unsigned char>(win[1]) == 0xBB &&
        static_cast<unsigned char>(win[2]) == 0xBF) {
        scan = ev_start = 3;
    }
    bool in_event = parse::is_event_start(win + (scan - win_base),
                                          static_cast<std::size_t>(win_base + win_len - scan));

    for (;;) {
        const std::uint64_t win_end = win_base + win_len;
        const bool at_eof = (win_end >= fsize);
        const std::uint64_t safe_end = at_eof ? win_end : win_end - kMapGuard;
        if (scan >= safe_end) {
            if (at_eof) break;
            // Двигаем окно вперёд, не теряя начала текущего события
            // (гигантское событие растит окно — прогресс гарантирован).
            if (!remap(ev_start, scan + map_bytes)) return false;
            continue;
        }
        const char* p = win + (scan - win_base);
        const void* nl = std::memchr(p, '\n', static_cast<std::size_t>(safe_end - scan));
        if (!nl) {
            scan = safe_end;
            if (at_eof) break;
            if (!remap(ev_start, scan + map_bytes)) return false;
            continue;
        }
        const std::uint64_t next_line =
            win_base + static_cast<std::uint64_t>(static_cast<const char*>(nl) - win) + 1;
        scan = next_line;
        if (next_line < fsize &&
            parse::is_event_start(win + (next_line - win_base),
                                  static_cast<std::size_t>(win_end - next_line))) {
            if (in_event) {
                emit(win + (ev_start - win_base),
                     static_cast<std::size_t>(next_line - ev_start));
            }
            in_event = true;
            ev_start = next_line;
        }
    }
    if (in_event && fsize > ev_start) {
        emit(win + (ev_start - win_base), static_cast<std::size_t>(fsize - ev_start));
    }
    return true;
}

} // namespace

NormalizerPipeline::NormalizerPipeline(Config cfg) : cfg_(std::move(cfg)) {}

std::size_t NormalizerPipeline::add_dir(const fs::path& dir) {
    std::size_t added = 0;
    std::error_code ec;
    fs::recursive_directory_iterator it(dir, fs::directory_options::skip_permission_denied, ec);
    if (ec) {
        ++failed_walk_;
        if (cfg_.on_error) {
            cfg_.on_error("Ошибка обхода директорий: " + dir.u8string() + ": " + ec.message());
        }
        return 0;
    }
    const fs::recursive_directory_iterator end_it;
    while (it != end_it) {
        const fs::directory_entry& entry = *it;
        bool regular = entry.is_regular_file(ec);
        if (!ec && regular && entry.path().extension() == ".log") {
            std::uint64_t size = entry.file_size(ec);
            if (ec) {
                ++failed_walk_;
                if (cfg_.on_error) {
                    cfg_.on_error("Ошибка чтения атрибутов " + entry.path().u8string() +
                                  ": " + ec.message());
                }
            } else if (size < parse::kMinFileSize) {
                ++small_file_skips_;
            } else {
                FileTask t;
                t.path = entry.path();
                t.size = size;
                t.date_prefix = util::date_from_filename(entry.path().filename().u8string());
                files_.push_back(std::move(t));
                ++added;
            }
        }
        it.increment(ec);
        if (ec) {
            ++failed_walk_;
            if (cfg_.on_error) {
                cfg_.on_error("Ошибка обхода директорий: " + ec.message());
            }
            break;
        }
    }
    return added;
}

RunStats NormalizerPipeline::run(const RecordFn& on_record, const FileFn& on_file) {
    return run_impl(false, on_record, {}, on_file);
}

RunStats NormalizerPipeline::run_rowbinary(const ChunkFn& on_chunk, const FileFn& on_file) {
    return run_impl(true, {}, on_chunk, on_file);
}

RunStats NormalizerPipeline::run_impl(bool rowbinary, const RecordFn& on_record,
                                      const ChunkFn& on_chunk, const FileFn& on_file) {
    // Сортировка по убыванию размера; stable — при равных размерах порядок
    // обхода (совпадает с Go-агентом; эталон при равенстве не специфицирован, §5).
    std::stable_sort(files_.begin(), files_.end(),
                     [](const FileTask& a, const FileTask& b) { return a.size > b.size; });

    unsigned workers = cfg_.workers != 0 ? cfg_.workers : std::thread::hardware_concurrency();
    if (workers < 1) workers = 1;
    if (workers > 1024) workers = 1024;
    const std::uint64_t budget =
        cfg_.admission_budget_bytes != 0
            ? cfg_.admission_budget_bytes
            : std::max<std::uint64_t>(kAdmissionBytesPerWorker * workers, kAdmissionBytesFloor);
    std::size_t chunk_bytes = cfg_.chunk_bytes != 0 ? cfg_.chunk_bytes : (4u << 20);
    std::uint64_t map_bytes = cfg_.map_bytes != 0 ? cfg_.map_bytes : (64u << 20);
    if (map_bytes < 4 * kMapGuard) map_bytes = 4 * kMapGuard; // окно ощутимо больше гварда

    const std::size_t n_files = files_.size();

    // Чанк готового вывода: байты + число записей (rows нужен RowBinary-приёмнику).
    struct Chunk {
        std::string data;
        std::uint64_t rows = 0;
    };
    // Слот файла: чанки + флаг завершения + списанный бюджет + телеметрия.
    struct FileSlot {
        std::vector<Chunk> chunks;
        bool done = false;
        std::uint64_t charged = 0; // байты, списанные с бюджета при допуске (0 у головы)
        FileCompletion comp;
    };
    std::vector<FileSlot> slots(n_files);

    std::mutex mx;
    std::condition_variable cv_workers; // будит воркеров (бюджет вернулся / голова сдвинулась)
    std::condition_variable cv_writer;  // будит писателя (появились чанки / файл готов)
    std::condition_variable cv_head;    // будит продюсера головы (писатель разгрузил очередь)
    std::size_t next_job = 0;
    std::size_t files_written = 0;      // писатель ждёт этот индекс — «голова»
    std::uint64_t admitted_bytes = 0;   // сумма charged допущенных неголовных файлов

    std::atomic<std::uint64_t> events{0}, parse_skips{0}, failed_files{0}, bytes{0};

    auto process_file = [&](std::size_t i) {
        FileSlot& slot = slots[i];
        FileCompletion comp;
        comp.path = files_[i].path.u8string();
        comp.bytes = files_[i].size;

        std::string buf;
        std::uint64_t buf_rows = 0;
        auto flush_chunk = [&]() {
            std::unique_lock<std::mutex> lk(mx);
            // Голова стримится с ограниченной очередью: писатель разгружает её
            // на лету, продюсер не копит вывод при медленном приёмнике.
            // Неголовной файл буферизует свой вывод свободно — его размер уже
            // списан с бюджета допуска. files_written не убывает и не может
            // перепрыгнуть i, пока файл не выдан целиком, поэтому предикат
            // «перестал быть головой» невозможен — ждём только разгрузки.
            cv_head.wait(lk, [&] {
                return files_written != i || slot.chunks.size() < kHeadMaxChunks;
            });
            slot.chunks.push_back(Chunk{std::move(buf), buf_rows});
            buf = std::string();
            buf_rows = 0;
            cv_writer.notify_all();
        };

        try {
            MappedWindowFile mf;
            if (!mf.open(files_[i].path)) {
                comp.ok = false;
                failed_files.fetch_add(1);
                if (cfg_.on_error) {
                    cfg_.on_error("Ошибка открытия файла: " + comp.path);
                }
            } else {
                bytes.fetch_add(mf.size());
                const std::string filename = files_[i].path.filename().u8string();
                const std::string file_path = util::rel_file_path(files_[i].path);

                // Контекст эмиссии: NDJSON — экранированные filename/file_path;
                // RowBinary — сырые + дата файла в µs.
                std::string filename_esc, file_path_esc;
                parse::RowBinaryCtx rbctx;
                if (rowbinary) {
                    rbctx.filename = filename;
                    rbctx.file_path = file_path;
                    parse::rb_init_date(rbctx, files_[i].date_prefix);
                } else {
                    parse::append_escaped(filename_esc, filename.data(), filename.size());
                    parse::append_escaped(file_path_esc, file_path.data(), file_path.size());
                }

                buf.reserve(chunk_bytes + (64u << 10));
                bool complete = scan_file_windowed(
                    mf, map_bytes, [&](const char* ev, std::size_t len) {
                        bool ok = rowbinary
                                      ? parse::append_event_rowbinary(buf, ev, len, rbctx)
                                      : parse::append_event(buf, ev, len,
                                                            files_[i].date_prefix,
                                                            filename_esc, file_path_esc);
                        if (ok) {
                            ++comp.events;
                            ++buf_rows;
                        } else {
                            ++comp.parse_skips;
                        }
                        if (buf.size() >= chunk_bytes) {
                            flush_chunk();
                            buf.reserve(chunk_bytes + (64u << 10));
                        }
                    });
                if (!complete) {
                    comp.ok = false;
                    failed_files.fetch_add(1);
                    if (cfg_.on_error) {
                        cfg_.on_error("Ошибка отображения файла: " + comp.path);
                    }
                }
                events.fetch_add(comp.events);
                parse_skips.fetch_add(comp.parse_skips);
            }
        } catch (const std::exception& e) {
            comp.ok = false;
            failed_files.fetch_add(1);
            if (cfg_.on_error) {
                cfg_.on_error("Ошибка обработки файла " + comp.path + ": " + e.what());
            }
        } catch (...) {
            comp.ok = false;
            failed_files.fetch_add(1);
            if (cfg_.on_error) {
                cfg_.on_error("Неизвестная ошибка обработки файла " + comp.path);
            }
        }

        // Финализация слота: остаток буфера + done — атомарно, писатель после
        // done со свопом чанков гарантированно видит всё. Без ожидания очереди
        // головы (один чанк сверх лимита не ломает модель, зато ошибочные пути
        // не блокируются).
        {
            std::lock_guard<std::mutex> lk(mx);
            if (!buf.empty()) slot.chunks.push_back(Chunk{std::move(buf), buf_rows});
            slot.comp = std::move(comp);
            slot.done = true;
            cv_writer.notify_all();
        }
    };

    // Воркеры: берут файлы строго по возрастанию индекса; голова — вне бюджета.
    std::vector<std::thread> pool;
    pool.reserve(workers);
    for (unsigned w = 0; w < workers; ++w) {
        pool.emplace_back([&]() {
            for (;;) {
                std::size_t i;
                {
                    std::unique_lock<std::mutex> lk(mx);
                    cv_workers.wait(lk, [&] {
                        if (next_job >= n_files) return true;
                        if (next_job == files_written) return true; // голова — безусловно
                        // admitted_bytes ≤ budget всегда (списываем только влезающее)
                        return files_[next_job].size <= budget - admitted_bytes;
                    });
                    if (next_job >= n_files) break;
                    i = next_job++;
                    if (i != files_written) {
                        admitted_bytes += files_[i].size;
                        slots[i].charged = files_[i].size;
                    }
                    // Допуск сдвинул next_job — соседи переоценивают предикат
                    // (следующий кандидат мог влезть в бюджет или стать головой).
                    cv_workers.notify_all();
                }
                // Страховка: исключение, ускользнувшее из process_file (bad_alloc
                // на путях вне его try, бросивший cfg_.on_error в catch-ветке),
                // не должно уронить поток std::terminate'ом — и слот обязан
                // получить done, иначе писатель ждёт вечно.
                try {
                    process_file(i);
                } catch (...) {
                    std::lock_guard<std::mutex> lk(mx);
                    if (!slots[i].done) {
                        slots[i].comp.ok = false;
                        slots[i].done = true;
                        failed_files.fetch_add(1);
                        cv_writer.notify_all();
                    }
                }
            }
        });
    }

    // Писатель — на потоке вызывающего: выдаёт записи строго в порядке файлов.
    std::exception_ptr sink_error;
    for (std::size_t i = 0; i < n_files; ++i) {
        bool done = false;
        while (!done) {
            std::vector<Chunk> got;
            {
                std::unique_lock<std::mutex> lk(mx);
                cv_writer.wait(lk, [&] { return slots[i].done || !slots[i].chunks.empty(); });
                got.swap(slots[i].chunks);
                done = slots[i].done;
            }
            cv_head.notify_all(); // очередь головы разгружена — продюсер может продолжать
            if (!sink_error) {
                try {
                    if (rowbinary) {
                        if (on_chunk) {
                            for (const Chunk& c : got) {
                                if (!c.data.empty()) {
                                    on_chunk(c.data.data(), c.data.size(), c.rows);
                                }
                            }
                        }
                    } else if (on_record) {
                        for (const Chunk& c : got) {
                            // Чанк — целые записи, каждая с завершающим '\n';
                            // выдаём по одной без '\n'.
                            const char* p = c.data.data();
                            const char* end = p + c.data.size();
                            while (p < end) {
                                const char* nl = static_cast<const char*>(
                                    std::memchr(p, '\n', static_cast<std::size_t>(end - p)));
                                if (!nl) nl = end; // не бывает: запись всегда терминирована
                                on_record(p, static_cast<std::size_t>(nl - p));
                                p = nl + 1;
                            }
                        }
                    }
                } catch (...) {
                    sink_error = std::current_exception(); // дальше только дренируем
                }
            }
            // got освобождается ЗДЕСЬ (до возврата бюджета): память чанков
            // реально свободна к моменту, когда допуск разрешит новые файлы.
        }
        if (!sink_error && on_file) {
            try {
                on_file(slots[i].comp);
            } catch (...) {
                sink_error = std::current_exception();
            }
        }
        {
            std::lock_guard<std::mutex> lk(mx);
            admitted_bytes -= slots[i].charged; // возврат бюджета — на любом пути
            slots[i].charged = 0;
            ++files_written; // голова сдвинулась
        }
        cv_workers.notify_all();
        cv_head.notify_all(); // новая голова могла ждать в flush_chunk с полной очередью
    }

    for (std::thread& t : pool) t.join();

    if (sink_error) std::rethrow_exception(sink_error);

    RunStats st;
    st.files = n_files;
    st.events = events.load();
    st.parse_skips = parse_skips.load();
    st.small_file_skips = small_file_skips_;
    st.failed_files = failed_files.load() + failed_walk_;
    st.bytes = bytes.load();
    return st;
}

} // namespace tj
