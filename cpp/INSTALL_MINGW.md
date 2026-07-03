# Установка MinGW-w64 для сборки C++ версии

Если у вас нет Visual Studio, используйте MinGW-w64 (бесплатный компилятор GCC для Windows).

## Быстрая установка

### Вариант 1: MSYS2 (рекомендуется)

1. **Скачайте MSYS2:**
   https://www.msys2.org/

2. **Установите MSYS2** (например, в `C:\msys64`)

3. **Откройте MSYS2 MinGW 64-bit** (через меню Пуск)

4. **Установите компилятор:**
   ```bash
   pacman -S mingw-w64-x86_64-gcc
   ```

5. **Добавьте в PATH:**
   ```
   C:\msys64\mingw64\bin
   ```
   
   Как добавить:
   - Нажмите Win + Pause
   - "Дополнительные параметры системы"
   - "Переменные среды"
   - В "Path" добавьте: `C:\msys64\mingw64\bin`

6. **Проверьте установку:**
   ```cmd
   g++ --version
   ```

### Вариант 2: Standalone MinGW-w64

1. **Скачайте:**
   https://github.com/niXman/mingw-builds-binaries/releases
   
   Выберите: `x86_64-*-release-posix-seh-ucrt-*.7z`

2. **Распакуйте** (например, в `C:\mingw64`)

3. **Добавьте в PATH:**
   ```
   C:\mingw64\bin
   ```

4. **Проверьте:**
   ```cmd
   g++ --version
   ```

## После установки

### Сборка C++ проекта:

```cmd
cd cpp
build_mingw.bat
```

Или через меню:
```cmd
cd cpp
build_simple.bat
```
Выберите опцию **3** (MinGW-w64)

### Запуск:

```cmd
cpp\run_cpp.bat "C:\TJ_Logs\TJ_Logs" 16
```

## Сравнение MSVC vs MinGW

| Параметр | MSVC | MinGW-w64 |
|----------|------|-----------|
| Скорость компиляции | Средняя | Быстрая |
| Скорость выполнения | Отлично | Хорошо (на 5-10% медленнее) |
| Размер exe | Маленький | Средний |
| Установка | Visual Studio (большая) | Легкая (~100MB) |
| Лицензия | Бесплатно для личного | Open Source |

**Рекомендация:** Для этого проекта MinGW-w64 вполне достаточно!

## Troubleshooting

### "g++ not found" после установки

Перезапустите командную строку после добавления в PATH.

### Ошибка "filesystem" при компиляции

Используйте флаг `-lstdc++fs`:
```cmd
g++ ... -lstdc++fs
```

Это уже добавлено в `build_mingw.bat`.

### Хотите Visual Studio?

Установите Visual Studio Community (бесплатно):
1. https://visualstudio.microsoft.com/downloads/
2. При установке выберите "Desktop development with C++"
3. После установки используйте `build_msvc.bat`

