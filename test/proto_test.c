/*
 * proto_test.c — WSP/1 protocol conformance tests (portable C99).
 *
 * Compiled on the host with:
 *   cc -std=c99 -Wall -Wextra -Werror -Ifirmware/main/include \
 *      test/proto_test.c firmware/main/wsp_proto.c -o build/proto_test
 *
 * The tests link the *real* firmware codec (firmware/main/wsp_proto.c) so a
 * regression in the wire format or the streaming parser fails here without
 * any ESP32 hardware.  No ESP-IDF, POSIX or third-party dependency is used.
 *
 * Exit code: 0 = all checks passed, 1 = at least one failure.
 */

#include "wsp_proto.h"

#include <stdint.h>
#include <stdio.h>
#include <string.h>

/* ------------------------------------------------------------------ */
/* Tiny test harness                                                  */
/* ------------------------------------------------------------------ */

static int g_checks;
static int g_failures;
static int g_section_base;

static void dump_hex(const char *label, const uint8_t *d, size_t n)
{
    size_t i;
    fprintf(stderr, "  %s (%zu bytes):", label, n);
    for (i = 0; i < n && i < 64; i++) {
        fprintf(stderr, " %02X", (unsigned)d[i]);
    }
    if (n > 64) {
        fprintf(stderr, " ...");
    }
    fprintf(stderr, "\n");
}

#define CHECK(cond)                                                          \
    do {                                                                     \
        ++g_checks;                                                          \
        if (!(cond)) {                                                       \
            ++g_failures;                                                    \
            fprintf(stderr, "FAIL %s:%d: %s\n", __FILE__, __LINE__, #cond);  \
        }                                                                    \
    } while (0)

#define CHECK_UEQ(actual, expected)                                          \
    do {                                                                     \
        unsigned long long _a = (unsigned long long)(actual);                \
        unsigned long long _e = (unsigned long long)(expected);              \
        ++g_checks;                                                          \
        if (_a != _e) {                                                      \
            ++g_failures;                                                    \
            fprintf(stderr, "FAIL %s:%d: %s = 0x%llX, want 0x%llX\n",        \
                    __FILE__, __LINE__, #actual, _a, _e);                    \
        }                                                                    \
    } while (0)

#define CHECK_SEQ(actual, expected)                                          \
    do {                                                                     \
        long long _a = (long long)(actual);                                  \
        long long _e = (long long)(expected);                                \
        ++g_checks;                                                          \
        if (_a != _e) {                                                      \
            ++g_failures;                                                    \
            fprintf(stderr, "FAIL %s:%d: %s = %lld, want %lld\n",            \
                    __FILE__, __LINE__, #actual, _a, _e);                    \
        }                                                                    \
    } while (0)

#define CHECK_MEMEQ(actual, expected, n)                                     \
    do {                                                                     \
        ++g_checks;                                                          \
        if (memcmp((actual), (expected), (size_t)(n)) != 0) {                \
            ++g_failures;                                                    \
            fprintf(stderr, "FAIL %s:%d: memcmp(%s, %s, %zu) differs\n",     \
                    __FILE__, __LINE__, #actual, #expected, (size_t)(n));    \
            dump_hex(#actual, (const uint8_t *)(actual), (size_t)(n));       \
            dump_hex(#expected, (const uint8_t *)(expected), (size_t)(n));   \
        }                                                                    \
    } while (0)

static void section_begin(void)
{
    g_section_base = g_failures;
}

static void section_end(const char *name)
{
    int failed = g_failures - g_section_base;
    if (failed == 0) {
        printf("[PASS] %s\n", name);
    } else {
        printf("[FAIL] %s (%d failed check(s))\n", name, failed);
    }
}

/* ------------------------------------------------------------------ */
/* Frame capture callback                                             */
/* ------------------------------------------------------------------ */

#define CAP_MAX_FRAMES 16

typedef struct {
    int count;
    struct {
        uint8_t type;
        uint8_t flags;
        uint16_t len;
        uint8_t payload[WSP_MAX_PAYLOAD];
    } f[CAP_MAX_FRAMES];
} capture_t;

static capture_t g_cap;

static void capture_reset(void)
{
    memset(&g_cap, 0, sizeof(g_cap));
}

static void capture_cb(void *ctx, uint8_t type, uint8_t flags,
                       const uint8_t *payload, uint16_t len)
{
    capture_t *c = (capture_t *)ctx;
    if (c->count < CAP_MAX_FRAMES) {
        c->f[c->count].type = type;
        c->f[c->count].flags = flags;
        c->f[c->count].len = len;
        if (len != 0 && payload != NULL) {
            memcpy(c->f[c->count].payload, payload, (size_t)len);
        }
    }
    c->count++;
}

/* Feed `data` split into chunks of at most `chunk` bytes.  Returns the last
 * non-OK code returned by wsp_parser_feed (WSP_OK if none). */
static int feed_chunks(wsp_parser_t *p, const uint8_t *data, size_t len,
                       size_t chunk, wsp_frame_cb cb, void *ctx)
{
    size_t off = 0;
    int last = WSP_OK;

    if (chunk == 0) {
        chunk = 1;
    }
    while (off < len) {
        size_t n = len - off;
        int rc;
        if (n > chunk) {
            n = chunk;
        }
        rc = wsp_parser_feed(p, data + off, n, cb, ctx);
        if (rc != WSP_OK) {
            last = rc;
        }
        off += n;
    }
    return last;
}

/* Encode one frame and return its size (0 on encoder failure). */
static size_t build_frame(uint8_t type, uint8_t flags, const uint8_t *payload,
                          uint16_t len, uint8_t *out, size_t cap)
{
    return wsp_encode(type, flags, payload, len, out, cap);
}

/* Deterministic xorshift32 PRNG so failures are reproducible. */
static uint32_t g_rng = 0x1234ABCDu;

static uint32_t rng_next(void)
{
    uint32_t x = g_rng;
    x ^= x << 13;
    x ^= x >> 17;
    x ^= x << 5;
    g_rng = x;
    return x;
}

/* ------------------------------------------------------------------ */
/* Test vectors from docs/protocol.md §2.2                            */
/* ------------------------------------------------------------------ */

static const uint8_t vec_data_payload[] = {0x48, 0x69};                 /* "Hi" */
static const uint8_t vec_data_frame[] = {0x57, 0x53, 0x12, 0x00, 0x02, 0x00,
                                         0x11, 0x0A, 0x48, 0x69};

static const uint8_t vec_hello_payload[] = {0x01, 0x00, 0x00, 0x04};
static const uint8_t vec_hello_frame[] = {0x57, 0x53, 0x10, 0x00, 0x04, 0x00,
                                          0x09, 0x37, 0x01, 0x00, 0x00, 0x04};

static const uint8_t vec_stats_frame[] = {0x57, 0x53, 0x17, 0x00, 0x00, 0x00,
                                          0xAF, 0x07};

static const uint8_t vec_ping_payload[] = {0x01, 0x02, 0x03, 0x04};
static const uint8_t vec_ping_frame[] = {0x57, 0x53, 0x18, 0x00, 0x04, 0x00,
                                         0xE0, 0x12, 0x01, 0x02, 0x03, 0x04};

typedef struct {
    const char *name;
    uint8_t type;
    const uint8_t *payload;
    uint16_t len;
    const uint8_t *frame;
    size_t frame_len;
} vector_t;

static const vector_t g_vectors[] = {
    {"DATA \"Hi\"", WSP_T_DATA, vec_data_payload, 2, vec_data_frame,
     sizeof(vec_data_frame)},
    {"HELLO", WSP_T_HELLO, vec_hello_payload, 4, vec_hello_frame,
     sizeof(vec_hello_frame)},
    {"STATS request", WSP_T_STATS, NULL, 0, vec_stats_frame,
     sizeof(vec_stats_frame)},
    {"PING", WSP_T_PING, vec_ping_payload, 4, vec_ping_frame,
     sizeof(vec_ping_frame)},
};

#define NUM_VECTORS (sizeof(g_vectors) / sizeof(g_vectors[0]))

static void test_crc_vector(void)
{
    static const uint8_t check[] = {'1', '2', '3', '4', '5', '6', '7', '8', '9'};
    uint8_t frame[WSP_MAX_FRAME];

    section_begin();

    /* CRC-16/CCITT-FALSE("123456789") = 0x29B1 */
    CHECK_UEQ(wsp_crc16(check, sizeof(check)), 0x29B1u);
    CHECK_UEQ(wsp_crc16(NULL, 0), 0xFFFFu);
    CHECK_UEQ(wsp_crc16(NULL, 3), 0u);

    /* The CRC field of every vector is stored little endian. */
    CHECK_UEQ(build_frame(WSP_T_STATS, 0, NULL, 0, frame, sizeof(frame)),
              sizeof(vec_stats_frame));
    CHECK_MEMEQ(frame, vec_stats_frame, sizeof(vec_stats_frame));

    section_end("CRC-16/CCITT-FALSE reference vector + STATS frame");
}

static void test_encode_vectors(void)
{
    size_t i;
    uint8_t frame[WSP_MAX_FRAME];

    section_begin();

    for (i = 0; i < NUM_VECTORS; i++) {
        const vector_t *v = &g_vectors[i];
        size_t n = build_frame(v->type, 0, v->payload, v->len, frame,
                               sizeof(frame));
        if (n != v->frame_len) {
            fprintf(stderr, "  vector %s: encoded %zu bytes, want %zu\n",
                    v->name, n, v->frame_len);
        }
        CHECK_UEQ(n, v->frame_len);
        CHECK_MEMEQ(frame, v->frame, v->frame_len);
    }

    section_end("encode matches protocol test vectors byte-for-byte");
}

static void test_decode_vectors(void)
{
    size_t i;

    section_begin();

    for (i = 0; i < NUM_VECTORS; i++) {
        const vector_t *v = &g_vectors[i];
        wsp_parser_t p;
        int rc;

        wsp_parser_init(&p);
        capture_reset();
        rc = feed_chunks(&p, v->frame, v->frame_len, 1, capture_cb, &g_cap);

        CHECK_SEQ(rc, WSP_OK);
        CHECK_UEQ(g_cap.count, 1);
        if (g_cap.count == 1) {
            CHECK_UEQ(g_cap.f[0].type, v->type);
            CHECK_UEQ(g_cap.f[0].flags, 0);
            CHECK_UEQ(g_cap.f[0].len, v->len);
            if (v->len != 0) {
                CHECK_MEMEQ(g_cap.f[0].payload, v->payload, v->len);
            }
        }
        CHECK_UEQ(p.stat_bad_crc, 0);
        CHECK_UEQ(p.stat_bad_version, 0);
    }

    section_end("streaming parser decodes all protocol test vectors");
}

/* ------------------------------------------------------------------ */
/* Encoder / decoder round trips                                      */
/* ------------------------------------------------------------------ */

static void test_roundtrip_lengths(void)
{
    static const uint16_t lengths[] = {0, 1, 2, 3, 7, 64, 255, 1023, 1024};
    static uint8_t payload[WSP_MAX_PAYLOAD];
    uint8_t frame[WSP_MAX_FRAME];
    size_t i;
    size_t k;

    section_begin();

    for (k = 0; k < sizeof(payload); k++) {
        payload[k] = (uint8_t)(k * 7u + 3u);
    }

    for (i = 0; i < sizeof(lengths) / sizeof(lengths[0]); i++) {
        uint16_t len = lengths[i];
        size_t n = build_frame(WSP_T_DATA, 0, payload, len, frame,
                               sizeof(frame));
        wsp_parser_t p;
        int rc;

        CHECK_UEQ(n, (size_t)WSP_HEADER_LEN + (size_t)len);
        CHECK_MEMEQ(frame, "\x57\x53", 2);

        wsp_parser_init(&p);
        capture_reset();
        rc = feed_chunks(&p, frame, n, 5, capture_cb, &g_cap);

        CHECK_SEQ(rc, WSP_OK);
        CHECK_UEQ(g_cap.count, 1);
        if (g_cap.count == 1) {
            CHECK_UEQ(g_cap.f[0].type, WSP_T_DATA);
            CHECK_UEQ(g_cap.f[0].len, len);
            if (len != 0) {
                CHECK_MEMEQ(g_cap.f[0].payload, payload, len);
            }
        }
        CHECK_UEQ(p.stat_bad_crc, 0);
        CHECK_UEQ(p.stat_bad_version, 0);
    }

    /* Encode capacity boundary: exactly enough is accepted, one less is not. */
    {
        size_t need = (size_t)WSP_HEADER_LEN + 1024u;
        uint8_t out[WSP_MAX_FRAME];
        CHECK_UEQ(wsp_encode(WSP_T_DATA, 0, payload, 1024, out, need), need);
        CHECK_UEQ(wsp_encode(WSP_T_DATA, 0, payload, 1024, out, need - 1), 0);
        CHECK_UEQ(wsp_encode(WSP_T_DATA, 0, payload, 0, out, WSP_HEADER_LEN),
                  WSP_HEADER_LEN);
        CHECK_UEQ(wsp_encode(WSP_T_DATA, 0, payload, 0, out, WSP_HEADER_LEN - 1),
                  0);
    }

    section_end("encode/decode round trip for payload lengths 0..1024");
}

static void test_encode_rejects(void)
{
    static uint8_t payload[WSP_MAX_PAYLOAD + 1];
    uint8_t frame[WSP_MAX_FRAME];

    section_begin();

    memset(payload, 0xA5, sizeof(payload));

    /* WSP_MAX_PAYLOAD + 1 = 1025 must be rejected. */
    CHECK_UEQ(wsp_encode(WSP_T_DATA, 0, payload, 1025, frame, sizeof(frame)), 0);
    /* 16-bit wrap guard: the API takes uint16_t, so test the boundary only. */
    CHECK_UEQ(wsp_encode(WSP_T_DATA, 0, payload, 1024, frame, sizeof(frame)),
              WSP_MAX_FRAME);

    /* Invalid arguments. */
    CHECK_UEQ(wsp_encode(WSP_T_DATA, 0, payload, 4, NULL, sizeof(frame)), 0);
    CHECK_UEQ(wsp_encode(WSP_T_DATA, 0, NULL, 4, frame, sizeof(frame)), 0);
    CHECK_UEQ(wsp_encode(WSP_T_DATA, 0, NULL, 0, frame, sizeof(frame)),
              WSP_HEADER_LEN);

    section_end("encoder rejects length 1025 and invalid arguments");
}

/* ------------------------------------------------------------------ */
/* Streaming parser behaviour                                         */
/* ------------------------------------------------------------------ */

static void test_stream_bytewise(void)
{
    static uint8_t payload[64];
    uint8_t frame[WSP_MAX_FRAME];
    size_t n;
    size_t i;
    wsp_parser_t p;
    int rc;

    section_begin();

    for (i = 0; i < sizeof(payload); i++) {
        payload[i] = (uint8_t)(0xF0u ^ i);
    }
    n = build_frame(WSP_T_DATA, 0, payload, (uint16_t)sizeof(payload), frame,
                    sizeof(frame));

    wsp_parser_init(&p);
    capture_reset();
    rc = feed_chunks(&p, frame, n, 1, capture_cb, &g_cap);

    CHECK_SEQ(rc, WSP_OK);
    CHECK_UEQ(g_cap.count, 1);
    CHECK_UEQ(g_cap.f[0].len, sizeof(payload));
    CHECK_MEMEQ(g_cap.f[0].payload, payload, sizeof(payload));

    section_end("byte-by-byte streaming parse (1-byte chunks)");
}

static void test_stream_chunk7(void)
{
    uint8_t stream[WSP_MAX_FRAME * 3];
    uint8_t payloads[3][WSP_MAX_PAYLOAD];
    uint16_t lens[3];
    size_t off = 0;
    size_t i;
    int j;
    wsp_parser_t p;
    int rc;

    section_begin();

    lens[0] = 0;
    lens[1] = 1;
    lens[2] = 1024;
    for (j = 0; j < 3; j++) {
        for (i = 0; i < lens[j]; i++) {
            payloads[j][i] = (uint8_t)(j * 31 + i);
        }
        off += build_frame(WSP_T_DATA, 0, payloads[j], lens[j], stream + off,
                           sizeof(stream) - off);
    }
    CHECK_UEQ(off, (size_t)WSP_HEADER_LEN * 3 + 0 + 1 + 1024);

    wsp_parser_init(&p);
    capture_reset();
    /* 7-byte chunks straddle the header/CRC/payload boundaries. */
    rc = feed_chunks(&p, stream, off, 7, capture_cb, &g_cap);

    CHECK_SEQ(rc, WSP_OK);
    CHECK_UEQ(g_cap.count, 3);
    for (j = 0; j < 3; j++) {
        CHECK_UEQ(g_cap.f[j].type, WSP_T_DATA);
        CHECK_UEQ(g_cap.f[j].len, lens[j]);
        if (lens[j] != 0) {
            CHECK_MEMEQ(g_cap.f[j].payload, payloads[j], lens[j]);
        }
    }
    CHECK_UEQ(p.stat_bad_crc, 0);
    CHECK_UEQ(p.stat_resync, 0);

    section_end("7-byte chunked parse across header/CRC/payload boundaries");
}

static void test_coalesced(void)
{
    uint8_t stream[WSP_MAX_FRAME * 4];
    static const uint8_t hello_pl[] = {0x01, 0x00, 0x00, 0x04};
    static const uint8_t data_pl[] = "coalesced-data";
    static const uint8_t ping_pl[] = {0xDE, 0xAD, 0xBE, 0xEF};
    size_t off = 0;
    wsp_parser_t p;
    int rc;

    section_begin();

    off += build_frame(WSP_T_HELLO, 0, hello_pl, sizeof(hello_pl), stream + off,
                       sizeof(stream) - off);
    off += build_frame(WSP_T_DATA, 0, data_pl, (uint16_t)(sizeof(data_pl) - 1),
                       stream + off, sizeof(stream) - off);
    off += build_frame(WSP_T_STATS, 0, NULL, 0, stream + off,
                       sizeof(stream) - off);
    off += build_frame(WSP_T_PING, 0, ping_pl, sizeof(ping_pl), stream + off,
                       sizeof(stream) - off);

    wsp_parser_init(&p);
    capture_reset();
    rc = wsp_parser_feed(&p, stream, off, capture_cb, &g_cap);

    CHECK_SEQ(rc, WSP_OK);
    CHECK_UEQ(g_cap.count, 4);
    if (g_cap.count == 4) {
        CHECK_UEQ(g_cap.f[0].type, WSP_T_HELLO);
        CHECK_UEQ(g_cap.f[1].type, WSP_T_DATA);
        CHECK_UEQ(g_cap.f[1].len, sizeof(data_pl) - 1);
        CHECK_MEMEQ(g_cap.f[1].payload, data_pl, sizeof(data_pl) - 1);
        CHECK_UEQ(g_cap.f[2].type, WSP_T_STATS);
        CHECK_UEQ(g_cap.f[2].len, 0);
        CHECK_UEQ(g_cap.f[3].type, WSP_T_PING);
        CHECK_MEMEQ(g_cap.f[3].payload, ping_pl, sizeof(ping_pl));
    }
    CHECK_UEQ(p.stat_resync, 0);

    section_end("coalesced frames (multiple frames in one TCP segment)");
}

static void test_garbage_resync(void)
{
    static const uint8_t garbage[] = {'a', 'b', 'c'};
    uint8_t frame[WSP_MAX_FRAME];
    uint8_t stream[WSP_MAX_FRAME + 32];
    size_t flen;
    size_t off;
    wsp_parser_t p;
    int rc;
    static uint8_t payload[] = "garbage-test";

    section_begin();

    flen = build_frame(WSP_T_DATA, 0, payload, (uint16_t)(sizeof(payload) - 1),
                        frame, sizeof(frame));

    /* 1. Garbage prefix in front of a valid frame. */
    off = 0;
    memcpy(stream + off, garbage, sizeof(garbage));
    off += sizeof(garbage);
    memcpy(stream + off, frame, flen);
    off += flen;

    wsp_parser_init(&p);
    capture_reset();
    rc = feed_chunks(&p, stream, off, 3, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_OK);
    CHECK_UEQ(g_cap.count, 1);
    CHECK_MEMEQ(g_cap.f[0].payload, payload, sizeof(payload) - 1);
    CHECK_UEQ(p.stat_resync, sizeof(garbage));

    /* 2. Garbage between two valid frames. */
    off = 0;
    memcpy(stream + off, frame, flen);
    off += flen;
    memcpy(stream + off, garbage, sizeof(garbage));
    off += sizeof(garbage);
    memcpy(stream + off, frame, flen);
    off += flen;

    wsp_parser_init(&p);
    capture_reset();
    rc = feed_chunks(&p, stream, off, 11, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_OK);
    CHECK_UEQ(g_cap.count, 2);
    CHECK_UEQ(p.stat_resync, sizeof(garbage));

    /* 3. A stray 'W' immediately followed by non-'S'. */
    off = 0;
    stream[off++] = 'W';
    stream[off++] = 'X';
    memcpy(stream + off, frame, flen);
    off += flen;

    wsp_parser_init(&p);
    capture_reset();
    rc = feed_chunks(&p, stream, off, 2, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_OK);
    CHECK_UEQ(g_cap.count, 1);
    CHECK_UEQ(p.stat_resync, 1);

    /* 4. "W W" then a junk byte restarts the magic hunt ("W W S" path). */
    off = 0;
    stream[off++] = 'W';
    stream[off++] = 'W';
    stream[off++] = 0x00;
    memcpy(stream + off, frame, flen);
    off += flen;

    wsp_parser_init(&p);
    capture_reset();
    rc = feed_chunks(&p, stream, off, 1, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_OK);
    CHECK_UEQ(g_cap.count, 1);
    CHECK_UEQ(p.stat_resync, 1);

    /* 5. "W W S" must restart as a complete bogus header (version mismatch)
     *    and then recover immediately for the following valid frame. */
    off = 0;
    stream[off++] = 'W';
    stream[off++] = 'W';
    stream[off++] = 'S';
    stream[off++] = 0x00;
    stream[off++] = 0x00;
    stream[off++] = 0x00;
    stream[off++] = 0x00;
    memcpy(stream + off, frame, flen);
    off += flen;

    wsp_parser_init(&p);
    capture_reset();
    rc = feed_chunks(&p, stream, off, 5, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_E_VERSION);
    CHECK_UEQ(g_cap.count, 1);
    CHECK_UEQ(p.stat_bad_version, 1);
    CHECK_UEQ(p.stat_resync, 0);

    section_end("garbage / magic resynchronisation");
}

static void test_crc_error_recovery(void)
{
    static uint8_t payload_a[] = "frame-A";
    static uint8_t payload_b[] = "frame-B-after-crc-error";
    uint8_t bad[WSP_MAX_FRAME];
    uint8_t good[WSP_MAX_FRAME];
    uint8_t stream[WSP_MAX_FRAME * 2];
    size_t blen;
    size_t glen;
    wsp_parser_t p;
    int rc;

    section_begin();

    blen = build_frame(WSP_T_DATA, 0, payload_a,
                       (uint16_t)(sizeof(payload_a) - 1), bad, sizeof(bad));
    glen = build_frame(WSP_T_DATA, 0, payload_b,
                       (uint16_t)(sizeof(payload_b) - 1), good, sizeof(good));
    /* Corrupt one payload byte of the first frame. */
    bad[WSP_HEADER_LEN] ^= 0xFFu;

    memcpy(stream, bad, blen);
    memcpy(stream + blen, good, glen);

    wsp_parser_init(&p);
    capture_reset();
    rc = wsp_parser_feed(&p, stream, blen + glen, capture_cb, &g_cap);

    CHECK_SEQ(rc, WSP_E_CRC);
    CHECK_UEQ(p.stat_bad_crc, 1);
    CHECK_UEQ(g_cap.count, 1);
    if (g_cap.count == 1) {
        CHECK_MEMEQ(g_cap.f[0].payload, payload_b, sizeof(payload_b) - 1);
    }

    /* A corrupted zero-length frame must recover as well. */
    wsp_parser_init(&p);
    capture_reset();
    {
        uint8_t bad0[16];
        memcpy(bad0, vec_stats_frame, sizeof(vec_stats_frame));
        bad0[6] ^= 0x01u;
        rc = wsp_parser_feed(&p, bad0, sizeof(vec_stats_frame), capture_cb,
                             &g_cap);
        CHECK_SEQ(rc, WSP_E_CRC);
        CHECK_UEQ(p.stat_bad_crc, 1);
        CHECK_UEQ(g_cap.count, 0);
        rc = wsp_parser_feed(&p, vec_stats_frame, sizeof(vec_stats_frame),
                             capture_cb, &g_cap);
        CHECK_SEQ(rc, WSP_OK);
        CHECK_UEQ(g_cap.count, 1);
        CHECK_UEQ(g_cap.f[0].type, WSP_T_STATS);
    }

    section_end("CRC error drops the frame and parsing continues");
}

static void test_version_error(void)
{
    static const uint8_t payload[] = {0x11, 0x22};
    uint8_t frame[WSP_MAX_FRAME];
    uint8_t bad[WSP_MAX_FRAME];
    size_t flen;
    wsp_parser_t p;
    int rc;

    section_begin();

    flen = build_frame(WSP_T_DATA, 0, payload, sizeof(payload), frame,
                       sizeof(frame));

    /* Version 2 in the high nibble must be rejected, not guessed. */
    memcpy(bad, frame, flen);
    bad[2] = (uint8_t)((2u << 4) | (bad[2] & 0x0Fu));

    wsp_parser_init(&p);
    capture_reset();
    rc = wsp_parser_feed(&p, bad, flen, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_E_VERSION);
    CHECK_UEQ(p.stat_bad_version, 1);
    CHECK_UEQ(p.stat_bad_crc, 0);
    CHECK_UEQ(g_cap.count, 0);

    /* Same with version 0. */
    bad[2] = (uint8_t)(0u << 4) | (frame[2] & 0x0Fu);
    rc = wsp_parser_feed(&p, bad, flen, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_E_VERSION);
    CHECK_UEQ(p.stat_bad_version, 2);

    /* Valid frame still parses after the version errors. */
    rc = wsp_parser_feed(&p, frame, flen, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_OK);
    CHECK_UEQ(g_cap.count, 1);
    CHECK_UEQ(g_cap.f[0].len, sizeof(payload));

    section_end("unsupported frame version rejected and counted");
}

static void test_length_error(void)
{
    uint8_t frame[WSP_MAX_FRAME];
    static const uint8_t payload[] = {0x01, 0x02, 0x03};
    size_t flen;
    wsp_parser_t p;
    int rc;
    uint8_t hdr[6];

    section_begin();

    flen = build_frame(WSP_T_DATA, 0, payload, sizeof(payload), frame,
                       sizeof(frame));

    wsp_parser_init(&p);
    capture_reset();

    /* Declared length 1025 (> WSP_MAX_PAYLOAD) must be rejected. */
    hdr[0] = 0x57;
    hdr[1] = 0x53;
    hdr[2] = 0x12;
    hdr[3] = 0x00;
    hdr[4] = 0x01; /* 1025 = 0x0401 */
    hdr[5] = 0x04;
    rc = wsp_parser_feed(&p, hdr, sizeof(hdr), capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_E_LENGTH);
    CHECK_UEQ(g_cap.count, 0);

    /* Declared length 0xFFFF as well. */
    hdr[4] = 0xFF;
    hdr[5] = 0xFF;
    rc = wsp_parser_feed(&p, hdr, sizeof(hdr), capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_E_LENGTH);

    /* Parser must recover and decode the next valid frame. */
    rc = wsp_parser_feed(&p, frame, flen, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_OK);
    CHECK_UEQ(g_cap.count, 1);
    CHECK_MEMEQ(g_cap.f[0].payload, payload, sizeof(payload));

    section_end("declared payload length > 1024 rejected, parser recovers");
}

static void test_stats_counters(void)
{
    uint8_t frame[WSP_MAX_FRAME];
    uint8_t bad[WSP_MAX_FRAME];
    static const uint8_t payload[] = {0xCA, 0xFE};
    size_t flen;
    wsp_parser_t p;
    int rc;

    section_begin();

    flen = build_frame(WSP_T_DATA, 0, payload, sizeof(payload), frame,
                       sizeof(frame));

    wsp_parser_init(&p);
    capture_reset();

    /* 3 garbage bytes -> stat_resync += 3 */
    rc = wsp_parser_feed(&p, (const uint8_t *)"xyz", 3, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_OK);
    CHECK_UEQ(p.stat_resync, 3);
    CHECK_UEQ(p.stat_bad_crc, 0);
    CHECK_UEQ(p.stat_bad_version, 0);

    /* 1 CRC error -> stat_bad_crc += 1 */
    memcpy(bad, frame, flen);
    bad[flen - 1] ^= 0x80u;
    rc = wsp_parser_feed(&p, bad, flen, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_E_CRC);
    CHECK_UEQ(p.stat_bad_crc, 1);

    /* 1 version error -> stat_bad_version += 1.  Feed exactly the 6-byte
     * header so no trailing bytes are re-scanned as garbage. */
    memcpy(bad, frame, 6);
    bad[2] = (uint8_t)((7u << 4) | (bad[2] & 0x0Fu));
    rc = wsp_parser_feed(&p, bad, 6, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_E_VERSION);
    CHECK_UEQ(p.stat_bad_version, 1);

    /* 2 valid frames -> callbacks only, counters unchanged. */
    rc = wsp_parser_feed(&p, frame, flen, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_OK);
    rc = wsp_parser_feed(&p, frame, flen, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_OK);

    CHECK_UEQ(g_cap.count, 2);
    CHECK_UEQ(p.stat_bad_crc, 1);
    CHECK_UEQ(p.stat_bad_version, 1);
    CHECK_UEQ(p.stat_resync, 3);

    section_end("parser statistics (resync / bad CRC / bad version)");
}

static void test_partial_frames(void)
{
    static const uint8_t payload[] = "partial";
    uint8_t frame[WSP_MAX_FRAME];
    size_t flen;
    wsp_parser_t p;
    int rc;

    section_begin();

    flen = build_frame(WSP_T_DATA, 0, payload,
                       (uint16_t)(sizeof(payload) - 1), frame, sizeof(frame));

    /* Split exactly at the 6-byte header / CRC boundary, then feed the rest. */
    wsp_parser_init(&p);
    capture_reset();
    rc = wsp_parser_feed(&p, frame, 6, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_OK);
    CHECK_UEQ(g_cap.count, 0);
    rc = wsp_parser_feed(&p, frame + 6, flen - 6, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_OK);
    CHECK_UEQ(g_cap.count, 1);
    CHECK_MEMEQ(g_cap.f[0].payload, payload, sizeof(payload) - 1);

    /* Split after the CRC bytes, mid-payload. */
    wsp_parser_init(&p);
    capture_reset();
    rc = wsp_parser_feed(&p, frame, 10, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_OK);
    CHECK_UEQ(g_cap.count, 0);
    rc = wsp_parser_feed(&p, frame + 10, flen - 10, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_OK);
    CHECK_UEQ(g_cap.count, 1);
    CHECK_MEMEQ(g_cap.f[0].payload, payload, sizeof(payload) - 1);

    /* Re-initialising a parser mid-frame discards the partial frame. */
    wsp_parser_init(&p);
    capture_reset();
    rc = wsp_parser_feed(&p, frame, 5, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_OK);
    wsp_parser_init(&p);
    rc = wsp_parser_feed(&p, frame, flen, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_OK);
    CHECK_UEQ(g_cap.count, 1);

    section_end("partial frames across feed() boundaries");
}

static void test_null_args(void)
{
    wsp_parser_t p;
    capture_reset();

    section_begin();

    wsp_parser_init(NULL); /* must not crash */

    wsp_parser_init(&p);
    CHECK_SEQ(wsp_parser_feed(NULL, NULL, 0, capture_cb, &g_cap), WSP_E_NOMEM);
    CHECK_SEQ(wsp_parser_feed(&p, NULL, 3, capture_cb, &g_cap), WSP_E_NOMEM);
    CHECK_SEQ(wsp_parser_feed(&p, NULL, 0, capture_cb, &g_cap), WSP_OK);
    CHECK_UEQ(g_cap.count, 0);

    /* NULL callback: frames are consumed and discarded, no crash. */
    CHECK_SEQ(feed_chunks(&p, vec_stats_frame, sizeof(vec_stats_frame), 2,
                          NULL, NULL),
              WSP_OK);

    section_end("NULL argument handling");
}

static void test_flags_ignored_on_rx(void)
{
    /* Reserved flags must be ignored by the receiver (docs §8). */
    static const uint8_t payload[] = {0x42};
    uint8_t frame[WSP_MAX_FRAME];
    size_t flen;
    wsp_parser_t p;
    int rc;

    section_begin();

    flen = build_frame(WSP_T_DATA, 0x5A, payload, sizeof(payload), frame,
                       sizeof(frame));
    wsp_parser_init(&p);
    capture_reset();
    rc = wsp_parser_feed(&p, frame, flen, capture_cb, &g_cap);
    CHECK_SEQ(rc, WSP_OK);
    CHECK_UEQ(g_cap.count, 1);
    CHECK_UEQ(g_cap.f[0].flags, 0x5A);
    CHECK_MEMEQ(g_cap.f[0].payload, payload, sizeof(payload));

    section_end("reserved flags carried through and ignored on receive");
}

static void test_random_roundtrip(void)
{
    static uint8_t payload[WSP_MAX_PAYLOAD];
    uint8_t frame[WSP_MAX_FRAME];
    int iter;
    int failures_before = g_failures;

    section_begin();

    for (iter = 0; iter < 200 && g_failures == failures_before; iter++) {
        uint16_t len = (uint16_t)(rng_next() % (WSP_MAX_PAYLOAD + 1u));
        size_t chunk = 1u + (rng_next() % 17u);
        size_t i;
        size_t n;
        wsp_parser_t p;
        int rc;

        for (i = 0; i < len; i++) {
            payload[i] = (uint8_t)rng_next();
        }
        n = build_frame(WSP_T_DATA, 0, payload, len, frame, sizeof(frame));
        CHECK_UEQ(n, (size_t)WSP_HEADER_LEN + (size_t)len);

        wsp_parser_init(&p);
        capture_reset();
        rc = feed_chunks(&p, frame, n, chunk, capture_cb, &g_cap);

        CHECK_SEQ(rc, WSP_OK);
        CHECK_UEQ(g_cap.count, 1);
        if (g_cap.count == 1) {
            CHECK_UEQ(g_cap.f[0].len, len);
            CHECK_UEQ(g_cap.f[0].type, WSP_T_DATA);
            if (len != 0) {
                CHECK_MEMEQ(g_cap.f[0].payload, payload, len);
            }
        }
    }

    section_end("200 randomised round trips with random chunk sizes");
}

/* ------------------------------------------------------------------ */
/* main                                                               */
/* ------------------------------------------------------------------ */

int main(void)
{
    printf("WSP/1 protocol conformance tests (wsp_proto.c, portable C99)\n");
    printf("max payload = %u, header = %u bytes, version = %u\n\n",
           (unsigned)WSP_MAX_PAYLOAD, (unsigned)WSP_HEADER_LEN,
           (unsigned)WSP_VERSION);

    test_crc_vector();
    test_encode_vectors();
    test_decode_vectors();
    test_roundtrip_lengths();
    test_encode_rejects();
    test_stream_bytewise();
    test_stream_chunk7();
    test_coalesced();
    test_garbage_resync();
    test_crc_error_recovery();
    test_version_error();
    test_length_error();
    test_stats_counters();
    test_partial_frames();
    test_null_args();
    test_flags_ignored_on_rx();
    test_random_roundtrip();

    printf("\n----------------------------------------\n");
    printf("proto_test: %d checks, %d failure(s)\n", g_checks, g_failures);
    if (g_failures == 0) {
        printf("RESULT: PASS\n");
        return 0;
    }
    printf("RESULT: FAIL\n");
    return 1;
}
