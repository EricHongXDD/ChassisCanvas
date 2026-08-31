#define _POSIX_C_SOURCE 200809L

#include <dlfcn.h>
#include <errno.h>
#include <limits.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#define INPUT_SAMPLE_RATE 16000
#define OUTPUT_SAMPLE_RATE 8000
#define BYTES_PER_SAMPLE 2
#define MIN_DURATION_MS 1000
#define MAX_DURATION_MS 15000
#define MAX_FINGERPRINT_BYTES (64 * 1024)
#define LIBRARY_NAME "libacrcloud_extr_tool.so"
#define FALLBACK_LIBRARY_PATH "/data/casescreen/libacrcloud_extr_tool.so"

typedef int (*create_fingerprint_fn)(char *, int, char, char **);
typedef void (*acr_free_fn)(char *);
typedef void (*acr_init_fn)(void);

typedef struct options {
    const char *format;
    long sample_rate;
    long channels;
    long duration_ms;
    int have_format;
    int have_sample_rate;
    int have_channels;
    int have_duration;
} options;

static void secure_clear(void *buffer, size_t length) {
    volatile unsigned char *cursor = (volatile unsigned char *)buffer;
    while (length > 0) {
        *cursor++ = 0;
        --length;
    }
}

static int parse_long(const char *text, long minimum, long maximum, long *result) {
    char *end = NULL;
    long value;

    if (text == NULL || *text == '\0') {
        return -1;
    }
    errno = 0;
    value = strtol(text, &end, 10);
    if (errno != 0 || end == text || *end != '\0' || value < minimum || value > maximum) {
        return -1;
    }
    *result = value;
    return 0;
}

static int parse_options(int argc, char **argv, options *result) {
    int index;

    memset(result, 0, sizeof(*result));
    if (argc != 9) {
        return -1;
    }
    for (index = 1; index < argc; index += 2) {
        const char *name = argv[index];
        const char *value = argv[index + 1];
        if (strcmp(name, "--format") == 0 && !result->have_format) {
            result->format = value;
            result->have_format = 1;
        } else if (strcmp(name, "--sample-rate") == 0 && !result->have_sample_rate) {
            if (parse_long(value, INPUT_SAMPLE_RATE, INPUT_SAMPLE_RATE, &result->sample_rate) != 0) {
                return -1;
            }
            result->have_sample_rate = 1;
        } else if (strcmp(name, "--channels") == 0 && !result->have_channels) {
            if (parse_long(value, 1, 1, &result->channels) != 0) {
                return -1;
            }
            result->have_channels = 1;
        } else if (strcmp(name, "--duration-ms") == 0 && !result->have_duration) {
            if (parse_long(value, MIN_DURATION_MS, MAX_DURATION_MS, &result->duration_ms) != 0) {
                return -1;
            }
            result->have_duration = 1;
        } else {
            return -1;
        }
    }
    return result->have_format && result->have_sample_rate && result->have_channels &&
                   result->have_duration && strcmp(result->format, "pcm_s16le") == 0
               ? 0
               : -1;
}

static int read_exact_stdin(unsigned char *buffer, size_t expected) {
    size_t total = 0;
    int trailing;

    while (total < expected) {
        size_t count = fread(buffer + total, 1, expected - total, stdin);
        if (count == 0) {
            if (ferror(stdin)) {
                return -1;
            }
            break;
        }
        total += count;
    }
    if (total != expected) {
        return -1;
    }
    trailing = fgetc(stdin);
    if (trailing != EOF || ferror(stdin)) {
        return -1;
    }
    return 0;
}

static int16_t read_s16le(const unsigned char *buffer, size_t sample_index) {
    size_t offset = sample_index * BYTES_PER_SAMPLE;
    uint16_t value = (uint16_t)buffer[offset] | ((uint16_t)buffer[offset + 1] << 8);
    return (int16_t)value;
}

static void write_s16le(unsigned char *buffer, size_t sample_index, int16_t value) {
    size_t offset = sample_index * BYTES_PER_SAMPLE;
    uint16_t bits = (uint16_t)value;
    buffer[offset] = (unsigned char)(bits & 0xffu);
    buffer[offset + 1] = (unsigned char)(bits >> 8);
}

/*
 * 15 阶半带 FIR，Q15 系数总和严格等于 32768。先低通再二分之一抽取，
 * 避免 4 kHz 以上分量直接混叠到官方 8 kHz 声纹输入中。
 */
static int downsample_16k_to_8k(const unsigned char *input, size_t input_samples,
                                unsigned char *output, size_t output_samples) {
    static const int32_t coefficients[15] = {
        -64, 0, 527, 0, -2170, 0, 9865, 16452,
        9865, 0, -2170, 0, 527, 0, -64,
    };
    size_t output_index;

    if (input_samples / 2 != output_samples) {
        return -1;
    }
    for (output_index = 0; output_index < output_samples; ++output_index) {
        int64_t accumulator = 0;
        int tap;
        int64_t centre = (int64_t)(output_index * 2);
        for (tap = 0; tap < 15; ++tap) {
            int64_t source_index = centre + tap - 7;
            if (source_index >= 0 && (uint64_t)source_index < input_samples) {
                accumulator += (int64_t)read_s16le(input, (size_t)source_index) * coefficients[tap];
            }
        }
        accumulator += accumulator >= 0 ? 16384 : -16384;
        accumulator /= 32768;
        if (accumulator > INT16_MAX) {
            accumulator = INT16_MAX;
        } else if (accumulator < INT16_MIN) {
            accumulator = INT16_MIN;
        }
        write_s16le(output, output_index, (int16_t)accumulator);
    }
    return 0;
}

static int library_path(char *result, size_t capacity) {
    char executable[PATH_MAX];
    ssize_t length = readlink("/proc/self/exe", executable, sizeof(executable) - 1);
    char *separator;
    int written;

    if (length <= 0 || (size_t)length >= sizeof(executable)) {
        written = snprintf(result, capacity, "%s", FALLBACK_LIBRARY_PATH);
        return written > 0 && (size_t)written < capacity ? 0 : -1;
    }
    executable[length] = '\0';
    separator = strrchr(executable, '/');
    if (separator == NULL) {
        return -1;
    }
    *separator = '\0';
    written = snprintf(result, capacity, "%s/%s", executable, LIBRARY_NAME);
    return written > 0 && (size_t)written < capacity ? 0 : -1;
}

int main(int argc, char **argv) {
    options parsed;
    size_t input_samples;
    size_t input_bytes;
    size_t output_samples;
    size_t output_bytes;
    unsigned char *input = NULL;
    unsigned char *output = NULL;
    char *fingerprint = NULL;
    int fingerprint_length = 0;
    char path[PATH_MAX];
    void *library = NULL;
    create_fingerprint_fn create_fingerprint = NULL;
    acr_free_fn acr_free = NULL;
    acr_init_fn acr_init = NULL;
    int status = 1;

    if (parse_options(argc, argv, &parsed) != 0) {
        fprintf(stderr, "参数无效\n");
        return 2;
    }
    if ((parsed.duration_ms * INPUT_SAMPLE_RATE) % 1000 != 0) {
        fprintf(stderr, "采样时长无法换算为整数采样点\n");
        return 2;
    }
    input_samples = (size_t)(parsed.duration_ms * INPUT_SAMPLE_RATE / 1000);
    input_bytes = input_samples * BYTES_PER_SAMPLE;
    output_samples = input_samples / 2;
    output_bytes = output_samples * BYTES_PER_SAMPLE;
    if (input_samples == 0 || input_samples % 2 != 0 || input_bytes > 480000) {
        fprintf(stderr, "采样长度无效\n");
        return 2;
    }

    input = (unsigned char *)malloc(input_bytes);
    output = (unsigned char *)malloc(output_bytes);
    if (input == NULL || output == NULL) {
        fprintf(stderr, "内存不足\n");
        goto cleanup;
    }
    if (read_exact_stdin(input, input_bytes) != 0) {
        fprintf(stderr, "PCM 输入长度无效\n");
        goto cleanup;
    }
    if (downsample_16k_to_8k(input, input_samples, output, output_samples) != 0) {
        fprintf(stderr, "PCM 降采样失败\n");
        goto cleanup;
    }
    secure_clear(input, input_bytes);

    if (library_path(path, sizeof(path)) != 0) {
        fprintf(stderr, "无法确定官方声纹库路径\n");
        goto cleanup;
    }
    library = dlopen(path, RTLD_NOW | RTLD_LOCAL);
    if (library == NULL) {
        fprintf(stderr, "无法加载官方声纹库\n");
        goto cleanup;
    }
    *(void **)(&create_fingerprint) = dlsym(library, "create_fingerprint");
    *(void **)(&acr_free) = dlsym(library, "acr_free");
    *(void **)(&acr_init) = dlsym(library, "acr_init");
    if (create_fingerprint == NULL || acr_free == NULL) {
        fprintf(stderr, "官方声纹库接口不完整\n");
        goto cleanup;
    }
    if (acr_init != NULL) {
        acr_init();
    }
    fingerprint_length = create_fingerprint((char *)output, (int)output_bytes, 0, &fingerprint);
    secure_clear(output, output_bytes);
    if (fingerprint == NULL || fingerprint_length <= 0 || fingerprint_length > MAX_FINGERPRINT_BYTES) {
        fprintf(stderr, "官方声纹输出无效\n");
        goto cleanup;
    }
    if (fwrite(fingerprint, 1, (size_t)fingerprint_length, stdout) != (size_t)fingerprint_length ||
        fflush(stdout) != 0) {
        fprintf(stderr, "声纹输出失败\n");
        goto cleanup;
    }
    status = 0;

cleanup:
    if (fingerprint != NULL && acr_free != NULL) {
        secure_clear(fingerprint, fingerprint_length > 0 ? (size_t)fingerprint_length : 0);
        acr_free(fingerprint);
    }
    if (output != NULL) {
        secure_clear(output, output_bytes);
        free(output);
    }
    if (input != NULL) {
        secure_clear(input, input_bytes);
        free(input);
    }
    if (library != NULL) {
        dlclose(library);
    }
    secure_clear(path, sizeof(path));
    return status;
}
