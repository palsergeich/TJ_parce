// util.hpp — вспомогательные функции ядра (не публичный API).
#pragma once

#include <filesystem>
#include <string>

namespace tj {
namespace util {

// Имя файла YYMMDDHH.log → префикс "20YY-MM-DDTHH:" (format-spec §3, timestamp).
// Первые 8 символов обязаны быть цифрами; суффикс и диапазоны не проверяются
// («месяц 13» пройдёт, век зашит "20"). Иначе — пустая строка.
std::string date_from_filename(const std::string& name);

// «Ровно два уровня предков» фактического пути: <коллекция>\<process_pid>\<файл>.log
// (format-spec §3, file_path). Повторяет семантику fs::path::operator/ эталона;
// разделители нативные для ОС. Возвращает UTF-8.
std::string rel_file_path(const std::filesystem::path& p);

} // namespace util
} // namespace tj
