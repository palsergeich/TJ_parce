// follow.hpp — tail-режим (--follow) участника C++: непрерывное слежение за
// каталогом логов с вставкой в ClickHouse и чекпоинтами (унифицированный
// контракт follow для всех трёх участников bake-off).
//
// Оркестрация живёт в CLI-слое (разрешено контрактом): переиспользует разбор
// ядра tj_core (tj::parse::is_event_start / append_event_rowbinary) поверх
// БУФЕРИЗОВАННОГО ReadFile с запомненных смещений — оконный mmap batch-режима
// здесь не используется (файлы растут; обязательные share-флаги
// FILE_SHARE_READ|WRITE|DELETE соблюдаются).
#pragma once

#include <cstdint>
#include <string>

#include "clickhouse_sink.hpp"

namespace tj_cli {

struct FollowConfig {
    std::string input;         // --input <dir> (обязателен)
    std::string state_dir;     // --state <dir> (обязателен): чекпоинты
    std::string stop_file;     // --stop-file <path> (обязателен): грациозный стоп
    std::uint64_t poll_ms = 500;        // период опроса каталога/файлов
    std::uint64_t idle_close_ms = 2000; // закрытие события по тишине
    ClickHouseConfig ch;       // хост/порт/таблица + батч-политика
    std::string stats_json;    // --stats-json <path> (опционален, пишется на выходе)
};

// Блокирует вызывающий поток до появления stop-file (или фатальной ошибки).
// Возвращает exit-код процесса: 0 — грациозный стоп (дренаж + финальный флаш +
// чекпоинт + stats-json); 1 — фатальная ошибка (ClickHouse недоступен после
// bounded-ретраев, ошибка state-каталога, ...). Прогресс — в stderr, stdout
// не используется.
int run_follow(const FollowConfig& cfg);

} // namespace tj_cli
