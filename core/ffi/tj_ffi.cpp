// tj_ffi.cpp — реализация C ABI поверх tj::NormalizerPipeline.
// Все исключения перехватываются на границе (DLL никогда не роняет хост).
#include "tj_ffi.h"

#include <tj/normalizer.hpp>

#include <exception>
#include <filesystem>
#include <memory>
#include <mutex>
#include <new>
#include <string>

struct tj_pipeline {
    tj::NormalizerPipeline pipe;
    tj_sink_fn sink = nullptr;
    void* user_data = nullptr;
    tj_file_fn file_cb = nullptr;
    tj::RunStats stats{};
    // Ошибки приходят и из рабочих потоков (on_error) — копим под мьютексом.
    std::mutex err_mx;
    std::string last_error;
    // Стабильный буфер для tj_last_error (живёт до следующего tj_*-вызова).
    std::string err_snapshot;

    explicit tj_pipeline(tj::Config cfg) : pipe(std::move(cfg)) {}

    void set_error(const std::string& msg) {
        std::lock_guard<std::mutex> lk(err_mx);
        if (!last_error.empty()) last_error += "; ";
        last_error += msg;
    }
    void clear_error() {
        std::lock_guard<std::mutex> lk(err_mx);
        last_error.clear();
    }
};

extern "C" {

TJ_API tj_pipeline* tj_create(const tj_config* cfg, tj_sink_fn sink, void* user_data) {
    try {
        tj::Config c;
        if (cfg) {
            c.workers = cfg->workers;
            c.admission_window = cfg->admission_window;
            c.chunk_bytes = cfg->chunk_bytes;
            if (cfg->map_bytes != 0) c.map_bytes = cfg->map_bytes;
        }
        // unique_ptr: если пересоздание конвейера ниже бросит (bad_alloc),
        // объект не утекает — до release() владение у умного указателя.
        auto p = std::make_unique<tj_pipeline>(c);
        p->sink = sink;
        p->user_data = user_data;
        // on_error замыкается на созданный объект, поэтому конвейер
        // пересоздаётся уже с колбэком (Config хранит std::function).
        tj_pipeline* raw = p.get();
        c.on_error = [raw](const std::string& msg) { raw->set_error(msg); };
        p->pipe = tj::NormalizerPipeline(std::move(c));
        return p.release();
    } catch (...) {
        return nullptr;
    }
}

TJ_API int32_t tj_set_file_callback(tj_pipeline* p, tj_file_fn cb) {
    if (!p) return -1;
    p->file_cb = cb;
    return 0;
}

TJ_API int32_t tj_add_dir(tj_pipeline* p, const char* utf8_dir) {
    if (!p) return -1;
    if (!utf8_dir) {
        p->set_error("tj_add_dir: dir == NULL");
        return -1;
    }
    try {
        std::filesystem::path dir = std::filesystem::u8path(utf8_dir);
        std::error_code ec;
        if (!std::filesystem::is_directory(dir, ec) || ec) {
            p->set_error(std::string("tj_add_dir: не директория: ") + utf8_dir);
            return -1;
        }
        return static_cast<int32_t>(p->pipe.add_dir(dir));
    } catch (const std::exception& e) {
        p->set_error(std::string("tj_add_dir: ") + e.what());
        return -1;
    } catch (...) {
        p->set_error("tj_add_dir: неизвестная ошибка");
        return -1;
    }
}

TJ_API int32_t tj_run(tj_pipeline* p) {
    if (!p) return 1;
    try {
        tj::NormalizerPipeline::RecordFn on_record;
        if (p->sink) {
            tj_sink_fn sink = p->sink;
            void* ud = p->user_data;
            on_record = [sink, ud](const char* data, std::size_t len) { sink(ud, data, len); };
        }
        tj::NormalizerPipeline::FileFn on_file;
        if (p->file_cb) {
            tj_file_fn cb = p->file_cb;
            void* ud = p->user_data;
            on_file = [cb, ud](const tj::FileCompletion& fc) {
                cb(ud, fc.path.c_str(), fc.events, fc.parse_skips, fc.bytes,
                   fc.ok ? 1 : 0);
            };
        }
        p->stats = p->pipe.run(on_record, on_file);
        return p->stats.failed_files > 0 ? 2 : 0;
    } catch (const std::exception& e) {
        p->set_error(std::string("tj_run: ") + e.what());
        return 1;
    } catch (...) {
        p->set_error("tj_run: неизвестная ошибка");
        return 1;
    }
}

TJ_API int32_t tj_get_stats(const tj_pipeline* p, tj_stats* out) {
    if (!p || !out) return -1;
    out->files = p->stats.files;
    out->events = p->stats.events;
    out->parse_skips = p->stats.parse_skips;
    out->small_file_skips = p->stats.small_file_skips;
    out->failed_files = p->stats.failed_files;
    out->bytes = p->stats.bytes;
    return 0;
}

TJ_API const char* tj_last_error(const tj_pipeline* p) {
    if (!p) return "";
    // Копирование строки может бросить bad_alloc — исключение не должно
    // пересечь C ABI (обещание заголовка).
    try {
        auto* mp = const_cast<tj_pipeline*>(p);
        std::lock_guard<std::mutex> lk(mp->err_mx);
        mp->err_snapshot = mp->last_error;
        return mp->err_snapshot.c_str();
    } catch (...) {
        return "";
    }
}

TJ_API void tj_destroy(tj_pipeline* p) {
    delete p;
}

} // extern "C"
