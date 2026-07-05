#include "util.hpp"

namespace tj {
namespace util {

std::string date_from_filename(const std::string& name) {
    if (name.size() < 8) return std::string();
    for (int i = 0; i < 8; ++i) {
        if (name[i] < '0' || name[i] > '9') return std::string();
    }
    std::string out;
    out.reserve(14);
    out.append("20");
    out.append(name, 0, 2);
    out.push_back('-');
    out.append(name, 2, 2);
    out.push_back('-');
    out.append(name, 4, 2);
    out.push_back('T');
    out.append(name, 6, 2);
    out.push_back(':');
    return out;
}

std::string rel_file_path(const std::filesystem::path& p) {
    std::filesystem::path parent = p.parent_path();
    std::filesystem::path grandparent = parent.parent_path();
    return (grandparent.filename() / parent.filename() / p.filename()).u8string();
}

} // namespace util
} // namespace tj
