# Сборка C++ версии с SIMD оптимизациями

## Требования

- Компилятор с поддержкой C++17 и AVX2:
  - Windows: Visual Studio 2019+ (MSVC) или MinGW-w64
  - Linux: GCC 8+ или Clang 8+

## 🚀 Быстрый старт (Windows)

### Вариант 1: Интерактивное меню (рекомендуется)

```cmd
cd cpp
build_simple.bat
```

Выберите компилятор из меню:
1. MSVC (Visual Studio) - лучшая производительность
2. MinGW-w64 - если нет Visual Studio
3. CMake - если установлен CMake

### Вариант 2: Прямая сборка с MSVC

```cmd
cd cpp
build_msvc.bat
```

Автоматически найдёт Visual Studio и скомпилирует.

Если возникают ошибки с vcvarsall.bat, попробуйте:
```cmd
cd cpp
build_direct.bat
```

Этот вариант ищет компилятор напрямую без vcvarsall.bat.

### Вариант 3: Сборка с MinGW-w64

```cmd
cd cpp
build_mingw.bat
```

Требуется установленный MinGW-w64 в PATH.

### Вариант 4: CMake (если установлен)

```cmd
cd cpp
build.bat
```

## 🐧 Сборка на Linux

```bash
cd cpp
mkdir build && cd build

# С GCC
g++ -O3 -march=native -mavx2 -flto -std=c++17 -pthread \
    -o count_contexts ../count_contexts.cpp

# Или с CMake
cmake .. -DCMAKE_BUILD_TYPE=Release
cmake --build . -j$(nproc)
```

## ▶️ Запуск

После успешной сборки:

```cmd
# Windows
cpp\run_cpp.bat "C:\TJ_Logs\TJ_Logs" 16

# Linux
./build/count_contexts "/path/to/logs" 16
```

Где:
- Первый аргумент - путь к папке с логами
- Второй аргумент (опционально) - количество потоков (по умолчанию = количество ядер CPU)

## 🔧 Устранение проблем

### Windows: "Visual Studio not found"

**Решение 1:** Установите Visual Studio Community (бесплатно)
- https://visualstudio.microsoft.com/downloads/
- Выберите "Desktop development with C++"

**Решение 2:** Используйте MinGW-w64
```cmd
# Скачайте и установите MinGW-w64
# https://sourceforge.net/projects/mingw-w64/
# Добавьте в PATH: C:\mingw64\bin
build_mingw.bat
```

### Windows: "g++ not found" (для MinGW)

Установите MinGW-w64 и добавьте в PATH:
```cmd
set PATH=%PATH%;C:\mingw64\bin
```

### Linux: "permission denied"

```bash
chmod +x build/count_contexts
```

### "AVX2 not supported"

Ваш процессор не поддерживает AVX2. Код автоматически переключится на обычный режим (будет работать, но медленнее).

Проверка поддержки AVX2:
- Windows: `wmic cpu get caption` → проверьте на ark.intel.com
- Linux: `grep avx2 /proc/cpuinfo`

### Низкая производительность

1. ✅ Убедитесь, что собрали в Release режиме
2. ✅ Проверьте поддержку AVX2 процессором
3. ✅ Увеличьте количество потоков
4. ✅ Используйте SSD вместо HDD
5. ✅ Для MSVC: используйте `build_msvc.bat` (быстрее чем MinGW)

## 📊 Флаги оптимизации

### MSVC (build_msvc.bat)
```
/O2      - Максимизация скорости
/GL      - Link-Time Code Generation
/Oi      - Встраивание функций
/Ot      - Оптимизация скорости над размером
/arch:AVX2 - Использование AVX2 инструкций
/fp:fast - Быстрая математика с плавающей точкой
/LTCG    - Link-Time Optimization
```

### GCC/MinGW (build_mingw.bat)
```
-O3           - Максимальная оптимизация
-march=native - Оптимизация под текущий CPU
-mavx2        - Использование AVX2
-flto         - Link-Time Optimization
-ffast-math   - Быстрая математика
```

## 🎯 Ожидаемая производительность

При оптимальных условиях (процессор с AVX2, NVMe SSD):

| Размер данных | Скорость обработки | Время (примерно) |
|--------------|-------------------|------------------|
| 1 GB         | 200-300 МБ/сек    | 3-5 секунд       |
| 10 GB        | 200-400 МБ/сек    | 30-50 секунд     |
| 100 GB       | 150-300 МБ/сек    | 5-10 минут       |

**Факторы, влияющие на скорость:**
- Скорость диска (SSD vs HDD: 5-10x разница)
- Поддержка AVX2 процессором (2-3x разница)
- Количество потоков
- Компилятор (MSVC обычно на 10-20% быстрее MinGW на Windows)

## 📚 Дополнительная информация

См. `README.md` в папке `cpp/` для:
- Подробного описания оптимизаций
- Сравнения с другими версиями
- Структуры кода
- Примеров использования
