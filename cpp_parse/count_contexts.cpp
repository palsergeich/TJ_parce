#include <iostream>
#include <fstream>
#include <string>
#include <string_view>
#include <vector>
#include <filesystem>
#include <algorithm>
#include <thread>
#include <mutex>
#include <atomic>
#include <chrono>
#include <cstring>
#include <cstdlib>
#include <immintrin.h> // AVX2
#include <condition_variable>
#include <memory>
#include <exception>
#include <numeric>

// Параллельные алгоритмы (C++17, может быть недоступно в старых компиляторах)
#if __has_include(<execution>)
    #include <execution>
    #define HAS_EXECUTION 1
#else
    #define HAS_EXECUTION 0
#endif

#ifdef _WIN32
#include <windows.h>
#include <memoryapi.h>
#include <intrin.h> // для _BitScanForward
#else
#include <sys/mman.h>
#include <unistd.h>
#endif

namespace fs = std::filesystem;

// Типы данных
// Используем обычный std::string (PMR требует аллокатор, что усложняет код без значительного выигрыша)
using String = std::string;

// Структура для метаданных файла
struct LogFileMeta {
    fs::path path;
    size_t size;
    std::string date_prefix; // "YYYY-MM-DDTHH:"
    
    LogFileMeta(fs::path p, size_t s, std::string d) 
        : path(std::move(p)), size(s), date_prefix(std::move(d)) {}
};

// Парсинг имени файла формата YYMMDDHH.log
// Пример: 25113021.log -> 2025-11-30T21:
std::string parse_date_from_filename(const std::string& filename) {
    if (filename.length() < 8) return ""; // Minimal length check
    
    // Проверяем, что первые 8 символов - цифры
    for (int i = 0; i < 8; ++i) {
        if (!isdigit(static_cast<unsigned char>(filename[i]))) return "";
    }
    
    std::string year = "20" + filename.substr(0, 2);
    std::string month = filename.substr(2, 2);
    std::string day = filename.substr(4, 2);
    std::string hour = filename.substr(6, 2);
    
    return year + "-" + month + "-" + day + "T" + hour + ":";
}

// MapType для хранения контекстов (удаляем, так как не используется для подсчета)
// template<typename K, typename V>
// using MapType = std::unordered_map<K, V>;

// Использование стандартных библиотек C++17:
// - std::string_view: избегание копирования строк при сравнении
// - std::execution: параллельные алгоритмы для сортировки больших данных
// - std::accumulate: эффективный подсчет сумм
// - std::equal: безопасное сравнение последовательностей

constexpr size_t MIN_FILE_SIZE = 100;
constexpr size_t LARGE_FILE_THRESHOLD = 50 * 1024 * 1024; // 50MB
constexpr size_t CHUNK_SIZE = 50 * 1024 * 1024; // 50MB
const char* CONTEXT_START = ",Context=";
constexpr size_t CONTEXT_START_LEN = 9;
constexpr char APOSTROPHE = '\'';
constexpr char NEWLINE = '\n';

// Маска начала события в технологическом журнале 1С: ЧЧ:ММ.СССССС-Длительность,
// Формат: две цифры часа, двоеточие, две цифры минуты, точка, шесть цифр, дефис, число (длительность), запятая
// Пример: 00:53.520012-0,
inline bool is_event_start(const char* line_start, size_t line_len) {
    // Проверяем минимальную длину: ЧЧ:ММ.СССССС-D, = 15 символов
    if (line_len < 15) return false;
    
    // Проверяем формат времени: ЧЧ:ММ.СССССС-
    if (!(line_start[0] >= '0' && line_start[0] <= '9' &&
          line_start[1] >= '0' && line_start[1] <= '9' &&
          line_start[2] == ':' &&
          line_start[3] >= '0' && line_start[3] <= '9' &&
          line_start[4] >= '0' && line_start[4] <= '9' &&
          line_start[5] == '.' &&
          line_start[6] >= '0' && line_start[6] <= '9' &&
          line_start[7] >= '0' && line_start[7] <= '9' &&
          line_start[8] >= '0' && line_start[8] <= '9' &&
          line_start[9] >= '0' && line_start[9] <= '9' &&
          line_start[10] >= '0' && line_start[10] <= '9' &&
          line_start[11] >= '0' && line_start[11] <= '9' &&
          line_start[12] == '-')) {
        return false;
    }
    
    // Проверяем длительность (число) и запятую после него
    size_t pos = 13;
    bool has_digits = false;
    
    // Оптимизация: проверяем сразу несколько символов, если это число
    while (pos < line_len) {
        char c = line_start[pos];
        if (c >= '0' && c <= '9') {
            has_digits = true;
            pos++;
        } else if (c == ',') {
            return has_digits; // Нашли запятую после цифр -> это событие
        } else {
            return false; // Не цифра и не запятая -> мусор -> не событие
        }
    }
    
    return false;
}

struct ContextInfo {
    String text;
    size_t count;
    
    ContextInfo() : count(0) {}
    ContextInfo(String t, size_t c) : text(std::move(t)), count(c) {}
    ContextInfo(std::string_view sv, size_t c) : text(sv), count(c) {}
};

// SIMD-оптимизированный хеш FNV-1a для больших данных
inline uint64_t hash_bytes_simd(const char* data, size_t len) {
    uint64_t hash = 0xcbf29ce484222325ULL;
    
#ifdef __AVX2__
    if (len >= 32) {
        const char* end = data + len;
        const char* ptr = data;
        
        // Обрабатываем по 32 байта через AVX2
        while (ptr + 32 <= end) {
            __m256i chunk = _mm256_loadu_si256(reinterpret_cast<const __m256i*>(ptr));
            
            // Извлекаем байты и хешируем
            alignas(32) uint8_t bytes[32];
            _mm256_store_si256(reinterpret_cast<__m256i*>(bytes), chunk);
            
            for (int i = 0; i < 32; ++i) {
                hash ^= static_cast<uint64_t>(bytes[i]);
                hash *= 0x100000001b3ULL;
            }
            
            ptr += 32;
        }
        
        // Остаток
        while (ptr < end) {
            hash ^= static_cast<uint64_t>(*ptr);
            hash *= 0x100000001b3ULL;
            ptr++;
        }
        
        return hash;
    }
#endif
    
    // Fallback для коротких данных
    for (size_t i = 0; i < len; ++i) {
        hash ^= static_cast<uint64_t>(data[i]);
        hash *= 0x100000001b3ULL;
    }
    return hash;
}

// Быстрый хеш FNV-1a (выбирает SIMD или обычный)
inline uint64_t hash_bytes(const char* data, size_t len) {
    return hash_bytes_simd(data, len);
}

// Вспомогательная функция для поиска первого установленного бита
inline int find_first_bit(unsigned int mask) {
    if (mask == 0) return -1;
#ifdef _WIN32
    unsigned long index;
    if (_BitScanForward(&index, mask)) {
        return static_cast<int>(index);
    }
    return -1;
#else
    return __builtin_ctz(mask);
#endif
}

// SIMD-оптимизированный поиск символа (для поиска \n)
inline const char* find_char_simd(const char* haystack, size_t haystack_len, char c) {
    if (haystack_len == 0) return nullptr;
    
#ifdef __AVX2__
    if (haystack_len >= 32) {
        __m256i target = _mm256_set1_epi8(c);
        const char* end = haystack + haystack_len;
        const char* ptr = haystack;
        
        while (ptr + 32 <= end) {
            // Prefetch следующего блока
            if (ptr + 64 < end) {
                _mm_prefetch(reinterpret_cast<const char*>(ptr + 64), _MM_HINT_T0);
            }
            
            __m256i chunk = _mm256_loadu_si256(reinterpret_cast<const __m256i*>(ptr));
            __m256i cmp = _mm256_cmpeq_epi8(chunk, target);
            int mask = _mm256_movemask_epi8(cmp);
            
            if (mask != 0) {
                int pos = find_first_bit(static_cast<uint32_t>(mask));
                if (pos >= 0) {
                    return ptr + pos;
                }
            }
            ptr += 32;
        }
        
        // Остаток
        while (ptr < end) {
            if (*ptr == c) return ptr;
            ptr++;
        }
        return nullptr;
    }
#endif
    
    // Fallback
    const char* result = static_cast<const char*>(memchr(haystack, c, haystack_len));
    return result;
}

// SIMD-оптимизированный поиск паттерна ',Context=' в начале строки
inline bool starts_with_pattern_simd(const char* line_start, size_t line_len) {
    if (line_len < CONTEXT_START_LEN) return false;
    
    // Для короткого паттерна (11 байт) обычный memcmp быстрее
    return memcmp(line_start, CONTEXT_START, CONTEXT_START_LEN) == 0;
}

// Подсчет количества вхождений символа (простая версия, чтобы исключить проблемы с инструкциями CPU)
inline size_t count_char_simd(const char* start, size_t len, char c) {
    size_t count = 0;
    const char* ptr = start;
    const char* end = start + len;
    while (ptr < end) {
        if (*ptr == c) count++;
        ptr++;
    }
    return count;
}

// Memory-mapped файл
class MappedFile {
private:
    const char* data_;
    size_t size_;
#ifdef _WIN32
    HANDLE file_handle_;
    HANDLE mapping_handle_;
#else
    int fd_;
#endif

public:
    MappedFile() : data_(nullptr), size_(0) {
#ifdef _WIN32
        file_handle_ = INVALID_HANDLE_VALUE;
        mapping_handle_ = NULL;
#else
        fd_ = -1;
#endif
    }
    
    ~MappedFile() {
        close();
    }
    
    // Запрещаем копирование, разрешаем перемещение
    MappedFile(const MappedFile&) = delete;
    MappedFile& operator=(const MappedFile&) = delete;
    
    MappedFile(MappedFile&& other) noexcept 
        : data_(other.data_), size_(other.size_) {
#ifdef _WIN32
        file_handle_ = other.file_handle_;
        mapping_handle_ = other.mapping_handle_;
        other.file_handle_ = INVALID_HANDLE_VALUE;
        other.mapping_handle_ = NULL;
#else
        fd_ = other.fd_;
        other.fd_ = -1;
#endif
        other.data_ = nullptr;
        other.size_ = 0;
    }
    
    MappedFile& operator=(MappedFile&& other) noexcept {
        if (this != &other) {
            close();
            data_ = other.data_;
            size_ = other.size_;
#ifdef _WIN32
            file_handle_ = other.file_handle_;
            mapping_handle_ = other.mapping_handle_;
            other.file_handle_ = INVALID_HANDLE_VALUE;
            other.mapping_handle_ = NULL;
#else
            fd_ = other.fd_;
            other.fd_ = -1;
#endif
            other.data_ = nullptr;
            other.size_ = 0;
        }
        return *this;
    }
    
    bool open(const fs::path& path) {
#ifdef _WIN32
        file_handle_ = CreateFileW(
            path.c_str(),
            GENERIC_READ,
            FILE_SHARE_READ,
            NULL,
            OPEN_EXISTING,
            FILE_ATTRIBUTE_NORMAL | FILE_FLAG_SEQUENTIAL_SCAN, // Оптимизация для последовательного чтения
            NULL
        );
        
        if (file_handle_ == INVALID_HANDLE_VALUE) {
            return false;
        }
        
        LARGE_INTEGER file_size;
        if (!GetFileSizeEx(file_handle_, &file_size)) {
            CloseHandle(file_handle_);
            return false;
        }
        size_ = static_cast<size_t>(file_size.QuadPart);
        
        if (size_ == 0) {
            CloseHandle(file_handle_);
            return true;
        }
        
        mapping_handle_ = CreateFileMappingW(
            file_handle_,
            NULL,
            PAGE_READONLY,
            0,
            0,
            NULL
        );
        
        if (mapping_handle_ == NULL) {
            CloseHandle(file_handle_);
            return false;
        }
        
        data_ = static_cast<const char*>(MapViewOfFile(
            mapping_handle_,
            FILE_MAP_READ,
            0,
            0,
            0
        ));
        
        if (data_ == nullptr) {
            CloseHandle(mapping_handle_);
            CloseHandle(file_handle_);
            return false;
        }
#else
        fd_ = ::open(path.c_str(), O_RDONLY);
        if (fd_ == -1) {
            return false;
        }
        
        struct stat st;
        if (fstat(fd_, &st) == -1) {
            ::close(fd_);
            return false;
        }
        size_ = st.st_size;
        
        if (size_ == 0) {
            ::close(fd_);
            return true;
        }
        
        data_ = static_cast<const char*>(mmap(
            nullptr,
            size_,
            PROT_READ,
            MAP_PRIVATE,
            fd_,
            0
        ));
        
        if (data_ == MAP_FAILED) {
            ::close(fd_);
            data_ = nullptr;
            return false;
        }
        
        // Hint для последовательного чтения
        madvise(const_cast<char*>(data_), size_, MADV_SEQUENTIAL);
#endif
        return true;
    }
    
    void close() {
        if (data_) {
#ifdef _WIN32
            UnmapViewOfFile(data_);
            if (mapping_handle_) CloseHandle(mapping_handle_);
            if (file_handle_ != INVALID_HANDLE_VALUE) CloseHandle(file_handle_);
#else
            munmap(const_cast<char*>(data_), size_);
            if (fd_ != -1) ::close(fd_);
#endif
            data_ = nullptr;
            size_ = 0;
        }
    }
    
    const char* data() const { return data_; }
    size_t size() const { return size_; }
};

// Оптимизированная функция добавления контекста - удалена
// Извлечение контекстов из буфера памяти - удалена

// Структура для события (Lightweight view)
struct EventView {
    const char* data; // Указатель на начало события
    size_t len;       // Длина
};

// Пакет событий (держит одну ссылку на файл для всех событий внутри)
struct EventBatch {
    std::shared_ptr<MappedFile> file;
    size_t file_index;
    std::string date_prefix;
    std::string filename;        // Имя файла (25113021.log)
    std::string file_path;       // Путь с родительской папкой (Mem\rphost_26236\25113021.log)
    std::vector<EventView> events;
    
    EventBatch() : file_index(0) {}
    
    EventBatch(std::shared_ptr<MappedFile> f, size_t fi, std::string dp, std::string fn, std::string fp)
        : file(std::move(f)), file_index(fi), date_prefix(std::move(dp)), 
          filename(std::move(fn)), file_path(std::move(fp)) {
        events.reserve(2000);
    }
    
    void clear() {
        events.clear();
        // file и meta сохраняются для переиспользования
    }
};

// Быстрая проверка, является ли строка числом.
// KI-2: строгая грамматика JSON-числа (RFC 8259): -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?
// Версии вида "8.3.22.1704", "1-2", ".5", "0.", "007" — строки, иначе выход не парсится как JSON.
inline bool is_number_token(std::string_view val) {
    if (val.empty() || val.length() > 32) return false;

    size_t i = 0;
    if (val[i] == '-') { if (++i == val.size()) return false; }

    // Целая часть: 0 или [1-9][0-9]*
    if (val[i] == '0') {
        ++i;
    } else if (val[i] >= '1' && val[i] <= '9') {
        while (i < val.size() && val[i] >= '0' && val[i] <= '9') ++i;
    } else {
        return false;
    }

    // Дробная часть
    if (i < val.size() && val[i] == '.') {
        ++i;
        if (i == val.size() || val[i] < '0' || val[i] > '9') return false;
        while (i < val.size() && val[i] >= '0' && val[i] <= '9') ++i;
    }

    // Экспонента
    if (i < val.size() && (val[i] == 'e' || val[i] == 'E')) {
        ++i;
        if (i < val.size() && (val[i] == '+' || val[i] == '-')) ++i;
        if (i == val.size() || val[i] < '0' || val[i] > '9') return false;
        while (i < val.size() && val[i] >= '0' && val[i] <= '9') ++i;
    }

    return i == val.size();
}

// Проверка: должно ли поле всегда быть строкой (независимо от содержимого)
inline bool is_always_string_field(std::string_view field_name) {
    return field_name == "SearchString" || 
           field_name == "Guid" || 
           field_name == "UUID";
}

// Функция экранирования JSON строк (с быстрым путем для обычного текста)
void json_escape(const std::string_view& s, std::string& out) {
    // Оценка размера
    size_t needed = out.size() + s.length() + 32;
    if (out.capacity() < needed) out.reserve(needed * 2);
    
    const char* data = s.data();
    size_t len = s.length();
    size_t start = 0;
    
    for (size_t i = 0; i < len; ++i) {
        unsigned char c = static_cast<unsigned char>(data[i]);
        
        // Быстрая проверка: если символ безопасный, пропускаем
        if (c >= 0x20 && c != '"' && c != '\\') {
            continue;
        }
        
        // Нашли спецсимвол: скидываем накопленный кусок
        if (i > start) {
            out.append(data + start, i - start);
        }
        
        // Экранируем спецсимвол
        switch (c) {
            case '"': out.append("\\\""); break;
            case '\\': out.append("\\\\"); break;
            case '\b': out.append("\\b"); break;
            case '\f': out.append("\\f"); break;
            case '\n': out.append("\\n"); break;
            case '\r': out.append("\\r"); break;
            case '\t': out.append("\\t"); break;
            default:
                // Управляющие символы < 0x20
                char buf[7];
                snprintf(buf, sizeof(buf), "\\u%04x", c);
                out.append(buf);
                break;
        }
        
        start = i + 1;
    }
    
    // Дописываем хвост
    if (start < len) {
        out.append(data + start, len - start);
    }
}

// Оптимизированная очередь с batch processing и lock-free операциями где возможно
template<typename T>
class OptimizedQueue {
private:
    std::vector<T> queue_;
    mutable std::mutex mutex_;
    std::condition_variable condition_;
    size_t max_size_;
    std::atomic<bool> finished_{false};
    std::atomic<size_t> size_{0};
    
public:
    explicit OptimizedQueue(size_t max_size = 5000) : max_size_(max_size) {
        queue_.reserve(max_size);
    }
    
    void push(T item) {
        std::unique_lock<std::mutex> lock(mutex_);
        condition_.wait(lock, [this] { 
            return queue_.size() < max_size_ || finished_.load(); 
        });
        if (!finished_.load()) {
            queue_.push_back(std::move(item));
            size_.store(queue_.size());
            condition_.notify_one();
        }
    }
    
    // Batch pop - берет несколько элементов за раз для уменьшения contention
    size_t pop_batch(std::vector<T>& batch, size_t batch_size = 10) {
        std::unique_lock<std::mutex> lock(mutex_);
        condition_.wait(lock, [this] { 
            return !queue_.empty() || finished_.load(); 
        });
        
        if (queue_.empty() && finished_.load()) {
            return 0;
        }
        
        size_t count = std::min(batch_size, queue_.size());
        batch.clear();
        batch.reserve(count);
        
        for (size_t i = 0; i < count; ++i) {
            batch.push_back(std::move(queue_[i]));
        }
        
        queue_.erase(queue_.begin(), queue_.begin() + count);
        size_.store(queue_.size());
        condition_.notify_all();
        return count;
    }
    
    bool pop(T& item) {
        std::vector<T> batch;
        if (pop_batch(batch, 1) > 0) {
            item = std::move(batch[0]);
            return true;
        }
        return false;
    }
    
    void finish() {
        std::lock_guard<std::mutex> lock(mutex_);
        finished_.store(true);
        condition_.notify_all();
    }
    
    bool is_finished() const {
        return finished_.load() && size_.load() == 0;
    }
    
    size_t size() const {
        return size_.load();
    }
};

// Глобальный mutex для вывода
static std::mutex g_cout_mutex;

// KI-5: флаг фатальной ошибки писателя (main обязан вернуть ненулевой код, а не зависнуть)
static std::atomic<bool> g_writer_failed{false};
// KI-12: счётчик файлов, которые не удалось открыть/замапить
static std::atomic<size_t> g_failed_files{0};

// Установка кодировки консоли на UTF-8 для Windows
#ifdef _WIN32
#include <io.h>
#include <fcntl.h>
#include <locale>
#include <codecvt>
#include <sstream>

// Вспомогательная функция для конвертации UTF-8 в UTF-16
std::wstring utf8_to_utf16(const std::string& utf8) {
    if (utf8.empty()) return std::wstring();
    int size_needed = MultiByteToWideChar(CP_UTF8, 0, utf8.c_str(), -1, NULL, 0);
    if (size_needed <= 0) return std::wstring();
    std::vector<wchar_t> wstr(size_needed);
    MultiByteToWideChar(CP_UTF8, 0, utf8.c_str(), -1, &wstr[0], size_needed);
    return std::wstring(wstr.data(), size_needed - 1);
}

// Вспомогательная функция для вывода UTF-8 строк в Windows консоль
inline void print_utf8(const std::string& str) {
    std::wstring wstr = utf8_to_utf16(str);
    DWORD written;
    WriteConsoleW(GetStdHandle(STD_OUTPUT_HANDLE), wstr.c_str(), static_cast<DWORD>(wstr.length()), &written, NULL);
}

inline void print_utf8_err(const std::string& str) {
    std::wstring wstr = utf8_to_utf16(str);
    DWORD written;
    WriteConsoleW(GetStdHandle(STD_ERROR_HANDLE), wstr.c_str(), static_cast<DWORD>(wstr.length()), &written, NULL);
}

// Макросы для удобного вывода
#define COUT_UTF8(msg) print_utf8(msg)
#define CERR_UTF8(msg) print_utf8_err(msg)

void setup_console_utf8() {
    // Устанавливаем кодовую страницу консоли на UTF-8 (65001)
    // Это должно быть вызвано ДО любого вывода в консоль
    SetConsoleOutputCP(65001);
    SetConsoleCP(65001);
}
#else
#define COUT_UTF8(msg) std::cout << msg
#define CERR_UTF8(msg) std::cerr << msg

void setup_console_utf8() {
    // На Linux/Unix UTF-8 обычно уже установлен по умолчанию
    try {
        std::locale utf8_locale("en_US.UTF-8");
        std::locale::global(utf8_locale);
        std::cout.imbue(utf8_locale);
        std::cerr.imbue(utf8_locale);
        std::cin.imbue(utf8_locale);
    } catch (...) {
        // Если UTF-8 locale недоступен, используем системный по умолчанию
    }
}
#endif


// Поток-писатель: пишет JSON строки в файл порциями
// KI-5: при фатальной ошибке писатель обязан дренировать очередь,
// иначе разборщики навсегда блокируются в push() и процесс виснет
static void drain_output_queue(OptimizedQueue<std::string>& output_queue) {
    std::vector<std::string> batch;
    batch.reserve(5000);
    while (output_queue.pop_batch(batch, 5000) != 0) {
        batch.clear();
    }
}

void writer_thread(
    OptimizedQueue<std::string>& output_queue,
    const fs::path& output_file) {

    try {
        // Создаем директории, если путь задан (и директория отсутствует)
        try {
            if (output_file.has_parent_path() && !output_file.parent_path().empty()) {
                fs::create_directories(output_file.parent_path());
            }
        } catch (const std::exception& e) {
            {
                std::lock_guard<std::mutex> lock(g_cout_mutex);
                std::cerr << "[Writer] ОШИБКА: не удалось создать директории для файла: "
                          << output_file << " (" << e.what() << ")" << std::endl;
            }
            g_writer_failed = true;
            drain_output_queue(output_queue);
            return;
        }

        std::ofstream out(output_file, std::ios::binary | std::ios::out);
        if (!out) {
            {
                std::lock_guard<std::mutex> lock(g_cout_mutex);
                std::cerr << "[Writer] ОШИБКА: не удалось открыть файл для записи: " << output_file << std::endl;
            }
            g_writer_failed = true;
            drain_output_queue(output_queue);
            return;
        }

        // KI-7: BOM в выходной NDJSON не пишем — jq/ClickHouse/Elastic его не переваривают

        // Ручной буфер для плавной записи (32 МБ)
        std::string write_buffer;
        write_buffer.reserve(32 * 1024 * 1024);
        
        std::vector<std::string> batch;
        batch.reserve(5000);
        
        while (true) {
            size_t count = output_queue.pop_batch(batch, 5000);
            if (count == 0) break;
            
            for (const auto& json_line : batch) {
                write_buffer.append(json_line);
                write_buffer.push_back('\n');
                
                // Пишем на диск, если буфер заполнен
                if (write_buffer.size() >= 32 * 1024 * 1024) {
                    out.write(write_buffer.data(), write_buffer.size());
                    write_buffer.clear();
                }
            }
            batch.clear();
        }
        
        // Пишем остатки буфера
        if (!write_buffer.empty()) {
            out.write(write_buffer.data(), write_buffer.size());
        }

        // Получаем размер файла
        size_t final_size = static_cast<size_t>(out.tellp());
        out.close();

        // KI-5: ошибка записи (диск полон и т.п.) должна дать ненулевой exit-код
        if (out.fail()) {
            std::lock_guard<std::mutex> lock(g_cout_mutex);
            std::cerr << "[Writer] ОШИБКА записи в файл (диск полон?): " << output_file << std::endl;
            g_writer_failed = true;
        }
        
        std::lock_guard<std::mutex> lock(g_cout_mutex);
        if (final_size == 0) {
            COUT_UTF8("ВНИМАНИЕ: Файл результатов пуст! Возможно, ничего не распарсилось.\n");
        } else {
            COUT_UTF8("Запись завершена. Размер файла: ");
            std::cout << (final_size / 1024.0 / 1024.0) << " МБ" << std::endl;
        }
        COUT_UTF8("Файл сохранен: ");
        // В Windows используем wstring для корректного вывода путей с кириллицей
#ifdef _WIN32
        COUT_UTF8(output_file.u8string());
#else
        std::cout << output_file;
#endif
        std::cout << std::endl;
        
    } catch (const std::exception& e) {
        {
            std::lock_guard<std::mutex> lock(g_cout_mutex);
            std::cerr << "[Writer] ОШИБКА: " << e.what() << std::endl;
        }
        g_writer_failed = true;
        drain_output_queue(output_queue); // KI-5: не оставляем разборщиков заблокированными
    } catch (...) {
        {
            std::lock_guard<std::mutex> lock(g_cout_mutex);
            std::cerr << "[Writer] НЕИЗВЕСТНАЯ ОШИБКА" << std::endl;
        }
        g_writer_failed = true;
        drain_output_queue(output_queue);
    }
}

// Поток-писатель-заглушка: просто потребляет очередь, не записывая в файл
void sink_thread(OptimizedQueue<std::string>& output_queue) {
    try {
        std::vector<std::string> batch;
        batch.reserve(5000);

        while (true) {
            size_t count = output_queue.pop_batch(batch, 5000);
            if (count == 0) break;
            batch.clear();
        }
    } catch (const std::exception& e) {
        std::lock_guard<std::mutex> lock(g_cout_mutex);
        std::cerr << "[Sink] ОШИБКА: " << e.what() << std::endl;
    } catch (...) {
        std::lock_guard<std::mutex> lock(g_cout_mutex);
        std::cerr << "[Sink] НЕИЗВЕСТНАЯ ОШИБКА" << std::endl;
    }
}

// Поток-читатель: читает файл построчно через ifstream (безопасно для больших файлов)
void reader_thread(
    const fs::path& file_path,
    size_t file_index,
    std::string date_prefix,
    OptimizedQueue<EventBatch>& event_queue) { // Используем EventBatch
    
    try {
        auto mapped_file = std::make_shared<MappedFile>();
        if (!mapped_file->open(file_path)) {
            g_failed_files.fetch_add(1); // KI-12: ошибка должна попасть в счётчик и exit-код
            std::lock_guard<std::mutex> lock(g_cout_mutex);
            std::cerr << "[Reader] Ошибка открытия файла (mmap): " << file_path << std::endl;
            return;
        }
        
        {
             // Отладочный вывод убран
        }
        
        // Пропускаем пустые файлы
        if (mapped_file->size() == 0) return;
        
        const char* data_start = mapped_file->data();
        if (data_start == nullptr) return;

        const char* data_end = data_start + mapped_file->size();

        // KI-6: пропускаем UTF-8 BOM, иначе первая строка не совпадает с маской
        // и первое событие файла молча теряется (все файлы ТЖ 1С начинаются с BOM)
        if (data_end - data_start >= 3 &&
            static_cast<unsigned char>(data_start[0]) == 0xEF &&
            static_cast<unsigned char>(data_start[1]) == 0xBB &&
            static_cast<unsigned char>(data_start[2]) == 0xBF) {
            data_start += 3;
        }

        const char* ptr = data_start;
        const char* event_start = data_start;

        bool in_event = false;
        if (data_end - data_start >= 15 && is_event_start(ptr, data_end - data_start)) {
            in_event = true;
        }
        
        size_t lines_processed = 0;
        size_t events_found = 0;
        
        // Извлекаем имя файла и путь с двумя родительскими папками
        std::string filename = file_path.filename().string();
        auto parent = file_path.parent_path();           // rphost_26236
        auto grandparent = parent.parent_path();         // Mem
        std::string file_path_relative = (grandparent.filename() / parent.filename() / file_path.filename()).string();
        
        // Создаем текущий батч
        EventBatch current_batch(mapped_file, file_index, date_prefix, filename, file_path_relative);
        
        // Основной цикл чтения (оптимизирован для CPU)
        while (ptr < data_end) {
            // Ищем следующую новую строку (SIMD ускорение)
            const char* newline = find_char_simd(ptr, data_end - ptr, '\n');

            if (newline) {
                lines_processed++;
                const char* line_start = ptr;
                size_t line_len = newline - line_start;

                // Переходим к следующей строке
                ptr = newline + 1;

                // Проверяем начало нового события в следующей строке
                if (ptr < data_end) {
                    size_t remaining = data_end - ptr;
                    if (remaining >= 15 && is_event_start(ptr, remaining)) { 
                        // Найдено начало нового события
                        if (in_event) {
                            events_found++;
                            size_t event_len = ptr - event_start;
                            current_batch.events.push_back({event_start, event_len});

                            if (current_batch.events.size() >= 2000) {
                                event_queue.push(current_batch); // Копируем структуру батча (shared_ptr инкрементится 1 раз)
                                current_batch.clear(); // Очищаем вектор событий, сохраняем ссылку на файл
                            }
                        }

                        // Начинаем новое событие
                        in_event = true;
                        event_start = ptr;
                    }
                }
            } else {
                ptr = data_end;
            }
        }
        
        // Последнее событие
        if (in_event) {
            events_found++;
            size_t event_len = data_end - event_start;
            if (event_len > 0) {
                current_batch.events.push_back({event_start, event_len});
            }
        }
        
        // Отправляем остаток
        if (!current_batch.events.empty()) {
            event_queue.push(std::move(current_batch));
        }
        
    } catch (const std::exception& e) {
        std::lock_guard<std::mutex> lock(g_cout_mutex);
        std::cerr << "[Reader " << file_index << "] ОШИБКА: " << e.what() << std::endl;
        std::cerr << "  Файл: " << file_path << std::endl;
    } catch (...) {
        std::lock_guard<std::mutex> lock(g_cout_mutex);
        std::cerr << "[Reader " << file_index << "] НЕИЗВЕСТНАЯ ОШИБКА" << std::endl;
        std::cerr << "  Файл: " << file_path << std::endl;
    }
}

// Поток-разборщик: обрабатывает события и формирует JSON
void parser_thread(
    OptimizedQueue<EventBatch>& event_queue,
    OptimizedQueue<std::string>& output_queue,
    std::atomic<size_t>& events_processed) {
    
    try {
        std::vector<EventBatch> batches;
        batches.reserve(100); // Берем пачку батчей (каждый содержит до 2000 событий)
        
        while (true) {
            size_t batch_count = event_queue.pop_batch(batches, 10);
            if (batch_count == 0) break;
            
            for (const auto& batch : batches) {
                // Прямой доступ к памяти файла через batch.file
                // (он жив, пока жива структура batch)
                
                for (const auto& event : batch.events) {
                    const char* ptr = event.data;
                    const char* end = event.data + event.len;
                    
                // Обрезаем завершающие \r\n, чтобы значения свойств не тянули CR/LF
                while (ptr < end && (end[-1] == '\n' || end[-1] == '\r')) {
                    --end;
                }
                
                    if (ptr >= end) continue;
                    
                    // 1. Парсим время и длительность
                    // Формат: ЧЧ:ММ.СССССС-Длительность,
                    const char* comma_pos = find_char_simd(ptr, end - ptr, ',');
                    if (!comma_pos) continue;
                    
                    // Ищем дефис между временем и длительностью (используем SIMD)
                    const char* dash_pos = find_char_simd(ptr, comma_pos - ptr, '-');
                    if (!dash_pos) continue;
                    
                    std::string_view time_part(ptr, dash_pos - ptr);
                    std::string_view duration_part(dash_pos + 1, comma_pos - (dash_pos + 1));

                    // KI-2: канонизация длительности — ведущие нули ("007") дают невалидный JSON
                    while (duration_part.size() > 1 && duration_part.front() == '0') {
                        duration_part.remove_prefix(1);
                    }
                    
                    // 2. Парсим имя события и уровень
                    // Формат: ,Event,Level,
                    const char* p = comma_pos + 1;
                    const char* next_comma = find_char_simd(p, end - p, ',');
                    if (!next_comma) continue;
                    
                    std::string_view event_name(p, next_comma - p);
                    
                    p = next_comma + 1;
                    next_comma = find_char_simd(p, end - p, ',');
                    
                    std::string_view level = "0";
                    if (next_comma) {
                        level = std::string_view(p, next_comma - p);
                        p = next_comma + 1;
                    } else {
                        level = std::string_view(p, end - p);
                        p = end;
                    }
                    
                    // 3. Формируем JSON
                    // Используем thread_local буфер для уменьшения аллокаций
                    static thread_local std::string json;
                    json.clear();
                    // Если буфер сильно разросся, можно его иногда "сжимать", но для производительности лучше не трогать
                    if (json.capacity() > 1024 * 1024) json.shrink_to_fit(); 
                    
                    json.reserve(event.len + 256); // Оценка размера с запасом
                    
                    // Пишем timestamp без промежуточной аллокации
                    json.append("{\"timestamp\":\"");
                    json.append(batch.date_prefix);
                    json.append(time_part);
                    json.append("\",\"duration\":");
                    json.append(duration_part);
                    json.append(",\"event\":\"");
                    json_escape(event_name, json);
                    json.append("\",\"level\":");
                    if (is_number_token(level)) {
                        json.append(level);
                    } else {
                        json.append("\"");
                        json_escape(level, json);
                        json.append("\"");
                    }
                    
                    // Добавляем имя файла и путь
                    json.append(",\"filename\":\"");
                    json_escape(batch.filename, json);
                    json.append("\",\"file_path\":\"");
                    json_escape(batch.file_path, json);
                    json.append("\"");
                    
                    // Парсим свойства (в линию, без вложенного объекта "p")
                    if (p < end) {
                        while (p < end) {
                            const char* eq_pos = find_char_simd(p, end - p, '=');
                            if (!eq_pos) break;

                            // Всегда добавляем запятую перед каждым свойством, т.к. базовые поля уже записаны
                            json.append(",");
                            
                            // Сохраняем имя свойства для проверки "всегда строковых" полей
                            std::string_view property_name(p, eq_pos - p);
                            
                            // Имя свойства
                            json.append("\"");
                            json_escape(property_name, json);
                            json.append("\":");
                            
                            p = eq_pos + 1;
                            if (p >= end) {
                                json.append("\"\"");
                                break;
                            }
                            
                            // Значение свойства
                            if (*p == '\'' || *p == '"') {
                                // Значение в кавычках (одинарных или двойных)
                                char quote_char = *p;
                                json.append("\"");
                                p++; // пропускаем открывающую кавычку
                                
                                const char* val_start = p;
                                
                                // Для одинарных: чётная кавычка + запятая = конец
                                // Для двойных: первая кавычка (экранирование "") + запятая = конец
                                if (quote_char == '\'') {
                                    // Одинарные кавычки: '' = экранирование, одиночная ' = конец
                                    bool closed = false;
                                    while (p < end) {
                                        const char* next_quote = find_char_simd(p, end - p, '\'');
                                        if (!next_quote) {
                                            // Не нашли закрывающую до конца
                                            json_escape(std::string_view(val_start, end - val_start), json);
                                            json.append("\"");
                                            p = end;
                                            closed = true;
                                            break;
                                        }
                                        p = next_quote;
                                        
                                        // Проверяем: это экранирование '' или закрывающая кавычка?
                                        if (p + 1 < end && p[1] == '\'') {
                                            // Экранирование '' -> одна кавычка в данных
                                            // Записываем текст до первой кавычки
                                            json_escape(std::string_view(val_start, p - val_start), json);
                                            json.append("'");  // В JSON это просто '
                                            p += 2;  // Пропускаем обе кавычки
                                            val_start = p;  // Продолжаем с новой позиции
                                        } else {
                                            // Это одиночная кавычка - кандидат на закрывающую
                                            // Проверяем: после неё запятая или конец?
                                            if (p + 1 == end || p[1] == ',') {
                                                // Это закрывающая кавычка
                                                json_escape(std::string_view(val_start, p - val_start), json);
                                                json.append("\"");
                                                p++;
                                                closed = true;
                                                break;
                                            } else {
                                                // Битый формат: одиночная кавычка внутри, за ней не запятая
                                                // Считаем её частью данных
                                                json_escape(std::string_view(val_start, p - val_start), json);
                                                json.append("'");
                                                p++;
                                                val_start = p;
                                            }
                                        }
                                    }
                                    // Если дошли до конца без закрывающей
                                    if (!closed) {
                                        json_escape(std::string_view(val_start, p - val_start), json);
                                        json.append("\"");
                                    }
                                } else {
                                    // Двойные кавычки: ищем закрывающую ", экранирование ""
                                    bool closed = false;
                                    while (p < end) {
                                        const char* next_quote = find_char_simd(p, end - p, '"');
                                        if (!next_quote) {
                                            // Не нашли закрывающую до конца
                                            json_escape(std::string_view(val_start, end - val_start), json);
                                            json.append("\"");
                                            p = end;
                                            closed = true;
                                            break;
                                        }
                                        p = next_quote;
                                        
                                        // Проверяем экранирование ""
                                        if (p + 1 < end && p[1] == '"') {
                                            // "" -> " (но в JSON это \")
                                            json_escape(std::string_view(val_start, p - val_start), json);
                                            json.append("\\\"");  // Экранированная кавычка для JSON
                                            p += 2;
                                            val_start = p;
                                            continue;
                                        }
                                        
                                        // Это закрывающая кавычка
                                        json_escape(std::string_view(val_start, p - val_start), json);
                                        json.append("\"");
                                        p++;
                                        closed = true;
                                        break;
                                    }
                                    // Если цикл завершился без break (не должно быть), закрываем
                                    if (!closed) {
                                        json_escape(std::string_view(val_start, p - val_start), json);
                                        json.append("\"");
                                    }
                                }
                            } else {
                                // Значение без кавычек (до запятой или конца)
                                // ВНИМАНИЕ: В ТЖ значения без кавычек могут содержать запятые только внутри кавычек, 
                                // но здесь мы ожидаем простые значения или числа.
                                // Однако, свойства разделяются запятыми.
                                const char* next_sep = find_char_simd(p, end - p, ',');
                                if (!next_sep) next_sep = end;
                                
                                std::string_view val(p, next_sep - p);
                                
                                // Проверяем: является ли это поле "всегда строковым" или числом
                                bool is_num = !is_always_string_field(property_name) && is_number_token(val);
                                if (is_num) {
                                    json.append(val);
                                } else {
                                    json.append("\"");
                                    json_escape(val, json);
                                    json.append("\"");
                                }
                                
                                p = next_sep;
                            }
                            
                            if (p < end && *p == ',') p++;
                        }
                    }
                    
                    json.append("}");
                    // Копируем в очередь (тут аллокация неизбежна, если не переделывать очередь на указатели)
                    output_queue.push(json); 
                    events_processed.fetch_add(1);
                }
            }
            
            batches.clear();
        }
    } catch (const std::exception& e) {
        std::lock_guard<std::mutex> lock(g_cout_mutex);
        std::cerr << "[Parser] ОШИБКА: " << e.what() << std::endl;
    } catch (...) {
        std::lock_guard<std::mutex> lock(g_cout_mutex);
        std::cerr << "[Parser] НЕИЗВЕСТНАЯ ОШИБКА" << std::endl;
    }
}

// Рекурсивный поиск .log файлов
std::vector<LogFileMeta> find_log_files(const fs::path& root) {
    std::vector<LogFileMeta> files;
    
    try {
        for (const auto& entry : fs::recursive_directory_iterator(root)) {
            if (entry.is_regular_file() && entry.path().extension() == ".log") {
                size_t size = entry.file_size();
                if (size >= MIN_FILE_SIZE) {
                    std::string filename = entry.path().filename().string();
                    std::string date_prefix = parse_date_from_filename(filename);
                    // Если не удалось распарсить дату, используем пустую строку (можно добавить логику fallback)
                    files.emplace_back(entry.path(), size, date_prefix);
                }
            }
        }
    } catch (const std::exception& e) {
        std::cerr << "Ошибка обхода директорий: " << e.what() << std::endl;
    }
    
    // Сортируем по размеру (большие первыми) - используем параллельную сортировку если доступно
#if HAS_EXECUTION
    if (files.size() > 1000) {
        std::sort(std::execution::par_unseq, files.begin(), files.end(), 
                  [](const auto& a, const auto& b) { return a.size > b.size; });
    } else {
        std::sort(files.begin(), files.end(), 
                  [](const auto& a, const auto& b) { return a.size > b.size; });
    }
#else
    std::sort(files.begin(), files.end(), 
              [](const auto& a, const auto& b) { return a.size > b.size; });
#endif
    
    return files;
}

// Устанавливаем handler для std::terminate, чтобы увидеть причину падения
void setup_terminate_handler() {
    std::set_terminate([]() {
        try {
            auto ex = std::current_exception();
            if (ex) std::rethrow_exception(ex);
        } catch (const std::exception& e) {
            std::lock_guard<std::mutex> lock(g_cout_mutex);
            std::cerr << "[FATAL] Unhandled exception: " << e.what() << std::endl;
        } catch (...) {
            std::lock_guard<std::mutex> lock(g_cout_mutex);
            std::cerr << "[FATAL] Unknown exception" << std::endl;
        }
        std::abort();
    });
}

int main(int argc, char* argv[]) {
    // ВАЖНО: Устанавливаем UTF-8 ПЕРЕД любым выводом в консоль!
    setup_console_utf8();
    setup_terminate_handler();
    
    try {
        COUT_UTF8("Запуск программы...\n");
        
        if (argc < 2) {
            CERR_UTF8("Использование: count_contexts <путь_к_папке> [workers] [output_file]\n");
            return 1;
        }
        
        fs::path directory_path = argv[1];
        
        // Проверяем существование директории
        if (!fs::exists(directory_path)) {
            CERR_UTF8("Ошибка: директория не существует: ");
            std::cerr << directory_path << std::endl;
            return 1;
        }
        
        if (!fs::is_directory(directory_path)) {
            CERR_UTF8("Ошибка: указанный путь не является директорией: ");
            std::cerr << directory_path << std::endl;
            return 1;
        }
        
        // KI-8: atoi + size_t превращали "-2" в ~1.8e19 потоков; валидируем явно
        size_t num_workers = std::thread::hardware_concurrency();
        if (argc >= 3) {
            char* end_ptr = nullptr;
            long w = std::strtol(argv[2], &end_ptr, 10);
            if (end_ptr == argv[2] || *end_ptr != '\0' || w < 1 || w > 1024) {
                CERR_UTF8("Ошибка: workers должен быть целым числом от 1 до 1024\n");
                return 1;
            }
            num_workers = static_cast<size_t>(w);
        }
        if (num_workers == 0) {
            num_workers = 1;
        }
        
        COUT_UTF8("Анализ папки: ");
        std::cout << directory_path << std::endl;
        std::cout << std::string(80, '=') << std::endl;
        COUT_UTF8("Поиск .log файлов...\n");
        
        auto files = find_log_files(directory_path);
        
        if (files.empty()) {
            COUT_UTF8("Не найдено .log файлов для обработки\n");
            return 0;
        }
    
    COUT_UTF8("Найдено файлов для обработки: ");
    std::cout << files.size() << std::endl;
    COUT_UTF8("Обработка файлов (");
    std::cout << num_workers;
    COUT_UTF8(" параллельных потоков)...\n");
    
    auto start_time = std::chrono::high_resolution_clock::now();
    
    // Новая архитектура: потоки-читатели и потоки-разборщики
    // Используем std::accumulate для подсчета общего размера
    size_t total_size = std::accumulate(
        files.begin(), files.end(), size_t(0),
        [](size_t sum, const auto& file) { return sum + file.size; });
    
    // Очередь событий между читателями и разборщиками
    // Хранит пакеты событий (EventBatch)
    OptimizedQueue<EventBatch> event_queue(300);
    
    // Очередь для вывода JSON строк
    OptimizedQueue<std::string> output_queue(5000);
        
        // Глобальные результаты
        std::mutex result_mutex;
    // std::unordered_map<std::string, size_t> all_contexts; // Больше не нужно
    std::atomic<size_t> events_processed{0};
    std::atomic<size_t> files_processed{0};
    
    // Количество потоков-читателей и разборщиков
    // Увеличиваем читателей до 8 (но не больше числа файлов и числа потоков)
    size_t num_readers = std::min<size_t>(8, std::min(files.size(), num_workers));
    if (num_readers == 0) num_readers = 1;
    
    size_t num_parsers = (num_workers > num_readers) ? (num_workers - num_readers) : 1;
    
    COUT_UTF8("Архитектура:\n");
    COUT_UTF8("  Потоков-читателей: ");
    std::cout << num_readers << std::endl;
    COUT_UTF8("  Потоков-разборщиков: ");
    std::cout << num_parsers << std::endl;
    std::cout << std::endl;
    
    // Распределяем файлы по читателям с балансировкой по размеру
    std::vector<std::vector<size_t>> reader_files(num_readers);
    std::vector<size_t> reader_sizes(num_readers, 0);
    
    for (size_t file_idx = 0; file_idx < files.size(); ++file_idx) {
        size_t best_reader = 0;
        size_t min_size = reader_sizes[0];
        
        for (size_t r = 1; r < num_readers; ++r) {
            if (reader_sizes[r] < min_size) {
                min_size = reader_sizes[r];
                best_reader = r;
            }
        }
        
        reader_files[best_reader].push_back(file_idx);
        reader_sizes[best_reader] += files[file_idx].size;
    }
    
    // Путь к выходному файлу
    fs::path output_file;
    if (argc >= 4) {
        output_file = argv[3];
    } else {
        // По умолчанию сохраняем в текущую директорию запуска
        output_file = fs::current_path() / "result.jsonl";
    }

    // Флаг отключения вывода в файл (4-й аргумент: --no-output / --no-write / --dry-run)
    bool no_output = false;
    if (argc >= 5) {
        std::string flag = argv[4];
        if (flag == "--no-output" || flag == "--no-write" || flag == "--dry-run") {
            no_output = true;
        }
    }
    
    COUT_UTF8("Файл результатов: ");
#ifdef _WIN32
    COUT_UTF8(output_file.u8string());
#else
    std::cout << output_file;
#endif
    std::cout << std::endl;
    if (no_output) {
        COUT_UTF8("Вывод в файл отключен (--no-output)\n");
    }
    
    // Запускаем поток-писатель или sink
    std::thread writer;
    if (no_output) {
        writer = std::thread(sink_thread, std::ref(output_queue));
    } else {
        writer = std::thread(writer_thread, std::ref(output_queue), std::ref(output_file));
    }
    
    // Запускаем потоки-читатели
    std::vector<std::thread> reader_threads;
    for (size_t i = 0; i < num_readers; ++i) {
        reader_threads.emplace_back([&, i]() {
            try {
                for (size_t file_idx : reader_files[i]) {
                    const auto& meta = files[file_idx];
                    // Отладочный вывод убран
                    reader_thread(meta.path, file_idx, meta.date_prefix, event_queue);
                    
                    size_t proc = files_processed.fetch_add(1) + 1;
                    // Вывод прогресса только раз в 50 файлов или в конце
                    if (proc % 50 == 0 || proc == files.size()) {
                        std::lock_guard<std::mutex> lock(g_cout_mutex);
                        std::cout << "\r";
                        COUT_UTF8("Прочитано: ");
                        std::cout << proc << "/" << files.size() 
                                  << " файлов (" << (proc * 100.0 / files.size()) << "%)" << std::flush;
                    }
                }
            } catch (const std::exception& e) {
                std::lock_guard<std::mutex> lock(g_cout_mutex);
                std::cerr << "\n[Reader Thread " << i << "] КРИТИЧЕСКАЯ ОШИБКА: " << e.what() << std::endl;
            } catch (...) {
                std::lock_guard<std::mutex> lock(g_cout_mutex);
                std::cerr << "\n[Reader Thread " << i << "] НЕИЗВЕСТНАЯ КРИТИЧЕСКАЯ ОШИБКА" << std::endl;
            }
        });
    }
    
    // Запускаем потоки-разборщики
    std::vector<std::thread> parser_threads;
    
    for (size_t i = 0; i < num_parsers; ++i) {
        parser_threads.emplace_back([&, i]() {
            try {
                // Отладочный вывод убран
                parser_thread(event_queue, output_queue, events_processed);
            } catch (const std::exception& e) {
                std::lock_guard<std::mutex> lock(g_cout_mutex);
                std::cerr << "\n[Parser Thread " << i << "] КРИТИЧЕСКАЯ ОШИБКА: " << e.what() << std::endl;
            } catch (...) {
                std::lock_guard<std::mutex> lock(g_cout_mutex);
                std::cerr << "\n[Parser Thread " << i << "] НЕИЗВЕСТНАЯ КРИТИЧЕСКАЯ ОШИБКА" << std::endl;
            }
        });
    }
    
        COUT_UTF8("Ожидание завершения читателей...\n");
        // Ждем завершения всех читателей
        for (size_t i = 0; i < reader_threads.size(); ++i) {
            if (reader_threads[i].joinable()) {
                reader_threads[i].join();
            }
        }
        std::cout << "\n";
        COUT_UTF8("Все читатели завершены. Событий в очереди: ");
        std::cout << event_queue.size() << std::endl;
        
        // Сигнализируем разборщикам, что чтение завершено
        event_queue.finish();
        COUT_UTF8("Сигнал завершения отправлен разборщикам\n");
        
        // Ждем завершения всех разборщиков
        COUT_UTF8("Ожидание завершения разборщиков...\n");
        for (size_t i = 0; i < parser_threads.size(); ++i) {
            if (parser_threads[i].joinable()) {
                parser_threads[i].join();
            }
        }
        COUT_UTF8("Все разборщики завершены. Ожидание записи...\n");
    
    // Сигнализируем писателю, что разбор завершен
    output_queue.finish();
    if (writer.joinable()) {
        writer.join();
    }
    
    // Результаты
    auto end_time = std::chrono::high_resolution_clock::now();
    auto duration = std::chrono::duration_cast<std::chrono::milliseconds>(end_time - start_time);
    
    std::cout << "\n\n" << std::string(80, '=') << std::endl;
    COUT_UTF8("Всего обработано событий: ");
    std::cout << events_processed << std::endl;
    COUT_UTF8("Обработано файлов: ");
    std::cout << (files.size() - g_failed_files.load()) << "/" << files.size() << std::endl;
    if (g_failed_files.load() > 0) {
        COUT_UTF8("Ошибок открытия файлов: ");
        std::cout << g_failed_files.load() << std::endl;
    }
    COUT_UTF8("Время обработки: ");
    std::cout << (duration.count() / 1000.0) << " секунд" << std::endl;
    COUT_UTF8("Общий размер файлов: ");
    std::cout << (total_size / (1024.0 * 1024.0)) << " МБ" << std::endl;
    COUT_UTF8("Скорость обработки: ");
    std::cout << (total_size / (1024.0 * 1024.0)) / (duration.count() / 1000.0) << " МБ/сек" << std::endl;
    std::cout << std::string(80, '=') << std::endl;
    
    if (!no_output && !g_writer_failed) {
        COUT_UTF8("Результаты сохранены в ");
#ifdef _WIN32
        COUT_UTF8(output_file.u8string());
#else
        std::cout << output_file;
#endif
        std::cout << std::endl;
    }

    // KI-5/KI-12: фатальные ошибки писателя и файлов -> ненулевой exit-код
    if (g_writer_failed) {
        CERR_UTF8("ОШИБКА: запись результатов не удалась, вывод неполный\n");
        return 1;
    }
    if (g_failed_files.load() > 0) {
        CERR_UTF8("ВНИМАНИЕ: часть файлов не обработана (см. счётчик ошибок)\n");
        return 2;
    }

    } catch (const std::exception& e) {
        std::cerr << "Критическая ошибка: " << e.what() << std::endl;
        return 1;
    } catch (...) {
        std::cerr << "Неизвестная критическая ошибка!" << std::endl;
        return 1;
    }

    return 0;
}

