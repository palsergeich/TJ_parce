/* selftest.c — минимальная проверка C ABI: линкуется с tj_core_ffi и
 * вызывает tj_create/tj_add_dir/tj_last_error/tj_get_stats/tj_destroy.
 * Чистый C (не C++) — заодно проверяет, что tj_ffi.h корректен как C-заголовок. */
#include <stdio.h>

#include "tj_ffi.h"

static unsigned long long g_records = 0;

static void sink(void* user_data, const char* record, size_t len) {
    (void)user_data;
    (void)record;
    (void)len;
    ++g_records;
}

int main(void) {
    tj_config cfg;
    tj_pipeline* p;
    tj_stats st;
    int rc;

    cfg.workers = 1;
    cfg.admission_window = 0;
    cfg.chunk_bytes = 0;
    cfg.map_bytes = 0;

    p = tj_create(&cfg, sink, NULL);
    if (!p) {
        printf("FAIL: tj_create вернул NULL\n");
        return 1;
    }
    if (tj_last_error(p)[0] != '\0') {
        printf("FAIL: непустая ошибка сразу после tj_create: %s\n", tj_last_error(p));
        tj_destroy(p);
        return 1;
    }

    /* Несуществующий каталог обязан дать -1 и текст ошибки, не крэш. */
    rc = tj_add_dir(p, "tj_ffi_selftest_no_such_dir_12345");
    if (rc != -1) {
        printf("FAIL: tj_add_dir(несуществующий) вернул %d, ожидался -1\n", rc);
        tj_destroy(p);
        return 1;
    }
    if (tj_last_error(p)[0] == '\0') {
        printf("FAIL: tj_last_error пуст после ошибки tj_add_dir\n");
        tj_destroy(p);
        return 1;
    }

    /* Прогон без файлов: корректный no-op, exit 0. */
    rc = tj_run(p);
    if (rc != 0) {
        printf("FAIL: tj_run без файлов вернул %d: %s\n", rc, tj_last_error(p));
        tj_destroy(p);
        return 1;
    }
    if (tj_get_stats(p, &st) != 0 || st.files != 0 || st.events != 0) {
        printf("FAIL: неожиданная статистика пустого прогона\n");
        tj_destroy(p);
        return 1;
    }
    if (g_records != 0) {
        printf("FAIL: sink вызван %llu раз на пустом прогоне\n", g_records);
        tj_destroy(p);
        return 1;
    }

    tj_destroy(p);
    tj_destroy(NULL); /* NULL допустим */

    printf("tj_ffi selftest OK\n");
    return 0;
}
