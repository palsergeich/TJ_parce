#include <iostream>
#include <fstream>
#include <string>
#include <string_view>
#include <vector>
#include <unordered_map>
#include <filesystem>
#include <algorithm>
#include <thread>
#include <mutex>
#include <atomic>
#include <chrono>
#include <cstring>
#include <immintrin.h> // AVX2
#include <queue>
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

// Подключаем memory_resource для PMR (C++17)
#if __has_include(<memory_resource>)
    #include <memory_resource>
    #define HAS_PMR 1
#else
    #define HAS_PMR 0
#endif

namespace fs = std::filesystem;

// Типы данных
// Используем обычный std::string (PMR требует аллокатор, что усложняет код без значительного выигрыша)
using String = std::string;

// MapType для хранения контекстов
template<typename K, typename V>
using MapType = std::unordered_map<K, V>;

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
            FILE_ATTRIBUTE_NORMAL,
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

// Оптимизированная функция добавления контекста с использованием string_view
inline void add_context_optimized(
    MapType<uint64_t, ContextInfo>& contexts,
    const char* context_data,
    size_t context_len) {
    
    if (context_len == 0) return;
    
    // Используем string_view для избежания копирования при сравнении
    std::string_view context_view(context_data, context_len);
    
    // Хешируем напрямую из буфера
    uint64_t hash = hash_bytes(context_data, context_len);
    
    auto it = contexts.find(hash);
    if (it != contexts.end()) {
        // Проверяем коллизию используя string_view для сравнения
        if (it->second.text.size() == context_len &&
            std::equal(context_view.begin(), context_view.end(), 
                      it->second.text.begin(), it->second.text.end())) {
            it->second.count++;
        } else {
            // Коллизия - добавляем \0 и перехешируем
            std::vector<char> new_data(context_data, context_data + context_len);
            new_data.push_back(0);
            hash = hash_bytes(new_data.data(), new_data.size());
            contexts[hash] = ContextInfo(String(context_view), 1);
        }
    } else {
        // Новый контекст - используем string_view для создания строки
        contexts[hash] = ContextInfo(String(context_view), 1);
    }
}

// Извлечение контекстов из буфера памяти для технологического журнала 1С
// События начинаются с маски времени ЧЧ:ММ.СССССС-, в каждом событии только один контекст
MapType<uint64_t, ContextInfo> extract_contexts_from_buffer(
    const char* data, size_t size) {
    
    MapType<uint64_t, ContextInfo> contexts;
    contexts.reserve(1000);
    
    bool in_event = false;           // Находимся внутри события
    bool in_context = false;         // Находимся внутри контекста
    bool context_found_in_event = false; // Контекст уже найден в текущем событии
    std::vector<std::pair<const char*, size_t>> context_lines; // Строки контекста
    context_lines.reserve(50);
    
    const char* line_start = data;
    const char* end = data + size;
    const char* p = data;
    
    while (p < end) {
        // Используем SIMD для поиска \n
        const char* next_newline = find_char_simd(p, end - p, '\n');
        if (next_newline == nullptr) {
            // Последняя строка (без \n в конце)
            next_newline = end;
        }
        
        const char* line_end = next_newline;
        size_t line_len = line_end - line_start;
        
        // Убираем \r\n
        while (line_len > 0 && (line_start[line_len - 1] == '\r' || 
                               line_start[line_len - 1] == '\n')) {
            line_len--;
        }
        
        if (line_len == 0) {
            line_start = next_newline + 1;
            p = line_start;
            continue;
        }
        
        // Проверяем начало нового события (маска времени ЧЧ:ММ.СССССС-)
        bool is_new_event = is_event_start(line_start, line_len);
        
        if (is_new_event) {
            // Новое событие - завершаем предыдущий контекст, если был
            if (in_context && !context_lines.empty()) {
                // Собираем контекст из указателей
                size_t total_size = 0;
                for (const auto& [ptr, len] : context_lines) {
                    total_size += len + 1; // +1 для \n
                }
                
                std::vector<char> context_bytes;
                context_bytes.reserve(total_size);
                
                for (size_t i = 0; i < context_lines.size(); ++i) {
                    if (i > 0) context_bytes.push_back('\n');
                    const auto& [ptr, len] = context_lines[i];
                    context_bytes.insert(context_bytes.end(), ptr, ptr + len);
                }
                
                add_context_optimized(contexts, context_bytes.data(), context_bytes.size());
                context_lines.clear();
            }
            
            // Начинаем новое событие
            in_event = true;
            in_context = false;
            context_found_in_event = false; // Сбрасываем флаг для нового события
            context_lines.clear();
            
            // Проверяем, есть ли контекст в этой же строке (после маски времени)
            // Ищем паттерн ',Context=' в строке
            if (line_len > CONTEXT_START_LEN) {
                // Ищем паттерн ',Context=' в строке (может быть не в начале)
                for (size_t pos = 0; pos <= line_len - CONTEXT_START_LEN; ++pos) {
                    if (memcmp(line_start + pos, CONTEXT_START, CONTEXT_START_LEN) == 0) {
                        // Нашли паттерн контекста - в событии только один контекст
                        in_context = true;
                        context_found_in_event = true; // Помечаем, что контекст найден
                        const char* content = line_start + pos + CONTEXT_START_LEN;
                        size_t content_len = line_len - pos - CONTEXT_START_LEN;
                        
                        if (content_len > 0 && content[content_len - 1] == APOSTROPHE) {
                            // Однострочный контекст
                            add_context_optimized(contexts, content, content_len - 1);
                            in_context = false;
                        } else {
                            context_lines.emplace_back(content, content_len);
                        }
                        break; // В событии только один контекст
                    }
                }
            }
        } else if (in_event) {
            // Строка внутри события
            if (in_context) {
            // Строка внутри события и внутри контекста
            if (line_len > 0 && line_start[line_len - 1] == APOSTROPHE) {
                // Конец контекста
                if (line_len > 1) {
                    context_lines.emplace_back(line_start, line_len - 1);
                }
                
                if (!context_lines.empty()) {
                    // Собираем контекст
                    size_t total_size = 0;
                    for (const auto& [ptr, len] : context_lines) {
                        total_size += len + 1;
                    }
                    
                    std::vector<char> context_bytes;
                    context_bytes.reserve(total_size);
                    
                    for (size_t i = 0; i < context_lines.size(); ++i) {
                        if (i > 0) context_bytes.push_back('\n');
                        const auto& [ptr, len] = context_lines[i];
                        context_bytes.insert(context_bytes.end(), ptr, ptr + len);
                    }
                    
                    add_context_optimized(contexts, context_bytes.data(), context_bytes.size());
                }
                
                context_lines.clear();
                in_context = false;
            } else {
                // Продолжение контекста
                context_lines.emplace_back(line_start, line_len);
            }
            } else {
                // В событии, но не в контексте - проверяем, не появился ли паттерн ',Context='
                // ТОЛЬКО если контекст еще не был найден в этом событии
                if (!context_found_in_event && line_len >= CONTEXT_START_LEN) {
                    // Ищем паттерн ',Context=' в строке
                    for (size_t pos = 0; pos <= line_len - CONTEXT_START_LEN; ++pos) {
                        if (memcmp(line_start + pos, CONTEXT_START, CONTEXT_START_LEN) == 0) {
                            // Нашли паттерн контекста - в событии только один контекст
                            in_context = true;
                            context_found_in_event = true; // Помечаем, что контекст найден
                            const char* content = line_start + pos + CONTEXT_START_LEN;
                            size_t content_len = line_len - pos - CONTEXT_START_LEN;
                            
                            if (content_len > 0 && content[content_len - 1] == APOSTROPHE) {
                                // Однострочный контекст
                                add_context_optimized(contexts, content, content_len - 1);
                                in_context = false;
                            } else {
                                context_lines.emplace_back(content, content_len);
                            }
                            break; // В событии только один контекст
                        }
                    }
                }
                // Если контекст уже был найден в событии, игнорируем все последующие паттерны
            }
        }
        
        line_start = next_newline + 1;
        p = line_start;
    }
    
    // Последний контекст (если файл не заканчивается новой строкой)
    if (in_context && !context_lines.empty()) {
        size_t total_size = 0;
        for (const auto& [ptr, len] : context_lines) {
            total_size += len + 1;
        }
        
        std::vector<char> context_bytes;
        context_bytes.reserve(total_size);
        
        for (size_t i = 0; i < context_lines.size(); ++i) {
            if (i > 0) context_bytes.push_back('\n');
            const auto& [ptr, len] = context_lines[i];
            context_bytes.insert(context_bytes.end(), ptr, ptr + len);
        }
        
        add_context_optimized(contexts, context_bytes.data(), context_bytes.size());
    }
    
    return contexts;
}

// Структура для события (Zero-Copy: указывает на память в MappedFile)
struct Event {
    std::shared_ptr<MappedFile> file; // Удерживает файл открытым
    const char* data;                 // Указатель на начало события в памяти
    size_t len;                       // Длина события
    size_t file_index;                // Индекс файла (для статистики)
    
    Event(std::shared_ptr<MappedFile> f, const char* d, size_t l, size_t fi)
        : file(std::move(f)), data(d), len(l), file_index(fi) {}
    
    Event() : data(nullptr), len(0), file_index(0) {}
};

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

// Поток-читатель: читает файл построчно через ifstream (безопасно для больших файлов)
void reader_thread(
    const fs::path& file_path,
    size_t file_index,
    OptimizedQueue<Event>& event_queue) {
    
    try {
        auto mapped_file = std::make_shared<MappedFile>();
        if (!mapped_file->open(file_path)) {
            std::lock_guard<std::mutex> lock(g_cout_mutex);
            std::cerr << "[Reader] Ошибка открытия файла (mmap): " << file_path << std::endl;
            return;
        }
        
        {
             // Отладочный вывод убран
        }
        
        // Пропускаем пустые файлы (они уже должны быть отфильтрованы, но проверяем для безопасности)
        if (mapped_file->size() == 0) {
            return; // Пустой файл - ничего не делаем
        }
        
        const char* data_start = mapped_file->data();
        if (data_start == nullptr) {
            return; // Файл не был замаплен (пустой или ошибка)
        }
        
        const char* data_end = data_start + mapped_file->size();
        const char* ptr = data_start;
        const char* event_start = data_start;
        
        // Проверяем, начинается ли файл с события (обычно да)
        bool in_event = false;
        if (mapped_file->size() >= 13 && is_event_start(ptr, mapped_file->size())) {
            in_event = true;
        }
        
        size_t lines_processed = 0;
        size_t events_found = 0;
        auto last_progress_time = std::chrono::steady_clock::now();
        
        // Batch для отправки событий
        std::vector<Event> event_batch;
        event_batch.reserve(100);
        
        while (ptr < data_end) {
            // Ищем следующую новую строку
            const char* newline = find_char_simd(ptr, data_end - ptr, '\n');
            
            if (newline) {
                lines_processed++;
                const char* line_start = ptr;
                size_t line_len = newline - line_start;
                
                // Переходим к следующей строке для следующей итерации
                ptr = newline + 1;
                
                // Проверяем начало НОВОГО события в СЛЕДУЮЩЕЙ строке (ptr указывает на начало следующей строки)
                if (ptr < data_end) {
                    size_t remaining = data_end - ptr;
                    if (remaining >= 13 && is_event_start(ptr, remaining)) {
                        // Найдено начало нового события.
                        // Текущее событие заканчивается на newline (включительно)
                        if (in_event) {
                            events_found++;
                            size_t event_len = ptr - event_start;
                            event_batch.emplace_back(mapped_file, event_start, event_len, file_index);
                            
                            if (event_batch.size() >= 100) {
                                for (auto& e : event_batch) event_queue.push(std::move(e));
                                event_batch.clear();
                            }
                        }
                        
                        // Начинаем новое событие
                        in_event = true;
                        event_start = ptr;
                    }
                }
            } else {
                // Конец файла без новой строки в конце
                ptr = data_end;
            }
            
            // Периодический вывод прогресса убран
            if (lines_processed % 100000 == 0) {
                 // ...
            }
        }
        
        // Последнее событие
        if (in_event) {
            events_found++;
            size_t event_len = data_end - event_start;
            if (event_len > 0) {
                event_batch.emplace_back(mapped_file, event_start, event_len, file_index);
            }
        }
        
        // Отправляем оставшиеся события
        for (auto& e : event_batch) {
            event_queue.push(std::move(e));
        }
        
        // Отладочный вывод убран
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

// Поток-разборщик: обрабатывает события и ищет контексты (Zero-Copy, точный парсинг)
void parser_thread(
    OptimizedQueue<Event>& event_queue,
    std::unordered_map<std::string, size_t>& local_contexts,
    std::mutex& result_mutex,
    std::atomic<size_t>& events_processed) {
    
    try {
        std::vector<Event> batch;
        batch.reserve(100);
        size_t local_processed = 0;
        auto last_progress_time = std::chrono::steady_clock::now();
        
        while (true) {
            size_t count = event_queue.pop_batch(batch, 100);
            if (count == 0) break;
            
            for (auto& event : batch) {
                const char* ptr = event.data;
                const char* end = event.data + event.len;
                
                // Ищем паттерн ',Context='
                while (ptr < end) {
                    // Оптимизация: ищем сначала запятую
                    const char* comma = find_char_simd(ptr, end - ptr, ',');
                    if (!comma) break; // Нет запятых -> нет контекста
                    
                    // Проверяем, что это Context=
                    // Нам нужно минимум 9 символов: ,Context=
                    if (comma + CONTEXT_START_LEN <= end && 
                        memcmp(comma, CONTEXT_START, CONTEXT_START_LEN) == 0) {
                        
                        // Нашли ,Context=. Проверяем, что дальше идет открывающая кавычка
                        const char* val_start_ptr = comma + CONTEXT_START_LEN;
                        if (val_start_ptr >= end || *val_start_ptr != '\'') {
                             ptr = comma + 1;
                             continue;
                        }
                        
                        // Нашли кандидата. Проверяем кавычки от начала события до запятой
                        size_t quotes = count_char_simd(event.data, comma - event.data, '\'');
                        if (quotes % 2 == 0) {
                            // Четное число кавычек -> мы вне строкового литерала -> ЭТО ОНО
                            const char* val_start = val_start_ptr + 1; // Пропускаем открывающую '
                            const char* val_ptr = val_start;
                            
                            // Ищем конец значения (закрывающая кавычка, но пропускаем '')
                            const char* val_end = nullptr;
                            while (val_ptr < end) {
                                const char* q = find_char_simd(val_ptr, end - val_ptr, '\'');
                                if (!q) break; // Не нашли закрывающую кавычку
                                
                                // Проверяем на '' (экранирование)
                                if (q + 1 < end && q[1] == '\'') {
                                    val_ptr = q + 2; // Пропускаем обе
                                    continue;
                                }
                                
                                // Нашли одиночную кавычку - конец
                                val_end = q;
                                break;
                            }
                            
                            if (val_end) {
                                // Формируем контекст
                                // Нужно убрать \r и заменить '' на '
                                // Используем string_view для работы с данными без копирования
                                std::string_view context_view(val_start, val_end - val_start);
                                std::string context_text;
                                context_text.reserve(context_view.size());
                                
                                // Обрабатываем символы: убираем \r и заменяем '' на '
                                for (size_t i = 0; i < context_view.size(); ++i) {
                                    if (context_view[i] == '\'') {
                                        if (i + 1 < context_view.size() && context_view[i + 1] == '\'') {
                                            context_text += '\''; // '' -> '
                                            ++i; // Пропускаем второй апостроф
                                            continue;
                                        }
                                    }
                                    if (context_view[i] != '\r') { // Пропускаем \r (но \n оставляем)
                                        context_text += context_view[i];
                                    }
                                }
                                
                                if (!context_text.empty()) {
                                    local_contexts[context_text]++;
                                }
                            }
                            
                            break; // Нашли контекст, выходим (один на событие)
                        }
                        // Если нечетное - ложное срабатывание (внутри чужого значения), ищем дальше
                    }
                    
                    ptr = comma + 1;
                }
                
                events_processed.fetch_add(1);
                local_processed++;
            }
            
            // Периодический вывод прогресса (только глобальный, чтобы не спамить)
            auto now = std::chrono::steady_clock::now();
            auto elapsed = std::chrono::duration_cast<std::chrono::seconds>(now - last_progress_time).count();
            if (local_processed % 10000 == 0 || elapsed >= 5) {
                if (local_processed > 0) {
                    last_progress_time = now;
                }
            }
            
            batch.clear();
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
std::vector<std::pair<fs::path, size_t>> find_log_files(const fs::path& root) {
    std::vector<std::pair<fs::path, size_t>> files;
    
    try {
        for (const auto& entry : fs::recursive_directory_iterator(root)) {
            if (entry.is_regular_file() && entry.path().extension() == ".log") {
                size_t size = entry.file_size();
                if (size >= MIN_FILE_SIZE) {
                    files.emplace_back(entry.path(), size);
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
                  [](const auto& a, const auto& b) { return a.second > b.second; });
    } else {
        std::sort(files.begin(), files.end(), 
                  [](const auto& a, const auto& b) { return a.second > b.second; });
    }
#else
    std::sort(files.begin(), files.end(), 
              [](const auto& a, const auto& b) { return a.second > b.second; });
#endif
    
    return files;
}

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
            CERR_UTF8("Использование: count_contexts <путь_к_папке> [workers]\n");
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
        
        size_t num_workers = (argc >= 3) ? std::atoi(argv[2]) : std::thread::hardware_concurrency();
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
        [](size_t sum, const auto& pair) { return sum + pair.second; });
    
        // Очередь событий между читателями и разборщиками (увеличен размер для batch processing)
        OptimizedQueue<Event> event_queue(1000); // Уменьшил до 1000, чтобы снизить потребление памяти
        
        // Глобальные результаты
        std::mutex result_mutex;
    std::unordered_map<std::string, size_t> all_contexts;
    std::atomic<size_t> events_processed{0};
    std::atomic<size_t> files_processed{0};
    
    // Количество потоков-читателей и разборщиков
    // Равное распределение: 50% на чтение, 50% на разбор
    size_t num_readers = num_workers / 2;
    if (num_readers == 0) num_readers = 1;
    // Ограничиваем количеством файлов, если файлов меньше
    if (num_readers > files.size()) num_readers = files.size();
    
    size_t num_parsers = num_workers - num_readers; 
    if (num_parsers == 0) num_parsers = 1;
    
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
        reader_sizes[best_reader] += files[file_idx].second;
    }
    
    // Запускаем потоки-читатели
    std::vector<std::thread> reader_threads;
    for (size_t i = 0; i < num_readers; ++i) {
        reader_threads.emplace_back([&, i]() {
            try {
                for (size_t file_idx : reader_files[i]) {
                    const auto& [path, size] = files[file_idx];
                    // Отладочный вывод убран
                    reader_thread(path, file_idx, event_queue);
                    
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
    std::vector<std::unordered_map<std::string, size_t>> parser_results(num_parsers);
    
    for (size_t i = 0; i < num_parsers; ++i) {
        parser_threads.emplace_back([&, i]() {
            try {
                // Отладочный вывод убран
                parser_thread(event_queue, parser_results[i], result_mutex, events_processed);
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
        COUT_UTF8("Все разборщики завершены\n");
    
    // Объединяем результаты разборщиков
    for (const auto& local_contexts : parser_results) {
        std::lock_guard<std::mutex> lock(result_mutex);
        for (const auto& [text, count] : local_contexts) {
            all_contexts[text] += count;
        }
    }
    
    auto end_time = std::chrono::high_resolution_clock::now();
    auto duration = std::chrono::duration_cast<std::chrono::milliseconds>(end_time - start_time);
    
    std::cout << "\n\n" << std::string(80, '=') << std::endl;
    COUT_UTF8("Найдено уникальных событий Context: ");
    std::cout << all_contexts.size() << std::endl;
    
    // Используем std::accumulate для подсчета общего количества
    size_t total_count = std::accumulate(
        all_contexts.begin(), all_contexts.end(), size_t(0),
        [](size_t sum, const auto& pair) { return sum + pair.second; });
    COUT_UTF8("Всего событий Context: ");
    std::cout << total_count << std::endl;
    COUT_UTF8("Обработано файлов: ");
    std::cout << files.size() << std::endl;
    COUT_UTF8("Время обработки: ");
    std::cout << (duration.count() / 1000.0) << " секунд" << std::endl;
    COUT_UTF8("Общий размер файлов: ");
    std::cout << (total_size / (1024.0 * 1024.0)) << " МБ" << std::endl;
    COUT_UTF8("Скорость обработки: ");
    std::cout << (total_size / (1024.0 * 1024.0)) / (duration.count() / 1000.0) << " МБ/сек" << std::endl;
    std::cout << std::string(80, '=') << std::endl;
    
    // Сохранение результатов
    fs::path output_file = fs::path(directory_path).parent_path() / "result_cpp.txt";
    std::cout << "\n";
    COUT_UTF8("Сохранение результатов в ");
    std::cout << output_file << "..." << std::endl;
    
    try {
        std::ofstream out(output_file, std::ios::out | std::ios::trunc);
        if (!out.is_open()) {
            std::cerr << "Ошибка создания файла результатов: " << output_file << std::endl;
            std::cerr << "Попытка сохранить в текущую директорию..." << std::endl;
            output_file = "result_cpp.txt";
            out.open(output_file, std::ios::out | std::ios::trunc);
            if (!out.is_open()) {
                std::cerr << "Критическая ошибка: не удалось создать файл результатов!" << std::endl;
                return 1;
            }
        }
    
    out << "Папка: " << directory_path << std::endl;
    out << "Обработано файлов: " << files.size() << std::endl;
    out << "Найдено уникальных событий Context: " << all_contexts.size() << std::endl;
    out << "Всего событий Context: " << total_count << std::endl;
    out << "Время обработки: " << (duration.count() / 1000.0) << " секунд" << std::endl;
    out << "Общий размер файлов: " << (total_size / (1024.0 * 1024.0)) << " МБ" << std::endl;
    out << "Скорость обработки: " 
        << (total_size / (1024.0 * 1024.0)) / (duration.count() / 1000.0) << " МБ/сек\n" << std::endl;
    
    // Список файлов
    out << "Обработанные файлы:" << std::endl;
    out << std::string(80, '-') << std::endl;
    for (const auto& [path, _] : files) {
        out << path.string() << std::endl;
    }
    out << std::endl;
    
    // Сортируем контексты по количеству - используем параллельную сортировку для больших данных
    std::vector<std::pair<std::string, size_t>> sorted_contexts(
        all_contexts.begin(), all_contexts.end());
    
#if HAS_EXECUTION
    if (sorted_contexts.size() > 1000) {
        std::sort(std::execution::par_unseq, sorted_contexts.begin(), sorted_contexts.end(),
                  [](const auto& a, const auto& b) {
                      return a.second > b.second || (a.second == b.second && a.first < b.first);
                  });
    } else {
        std::sort(sorted_contexts.begin(), sorted_contexts.end(),
                  [](const auto& a, const auto& b) {
                      return a.second > b.second || (a.second == b.second && a.first < b.first);
                  });
    }
#else
    std::sort(sorted_contexts.begin(), sorted_contexts.end(),
              [](const auto& a, const auto& b) {
                  return a.second > b.second || (a.second == b.second && a.first < b.first);
              });
#endif
    
    out << "Список уникальных контекстов с количеством:" << std::endl;
    out << std::string(80, '=') << std::endl;
    
        for (size_t i = 0; i < sorted_contexts.size(); ++i) {
            const auto& [text, count] = sorted_contexts[i];
            out << "\n" << (i + 1) << ". (встречается " << count << " раз)" << std::endl;
            out << text << std::endl;
            out << std::string(80, '-') << std::endl;
        }
        
        out.close();
        COUT_UTF8("Готово! Результаты сохранены в ");
        std::cout << output_file << std::endl;
    } catch (const std::exception& e) {
        std::cerr << "Ошибка при сохранении результатов: " << e.what() << std::endl;
        return 1;
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

