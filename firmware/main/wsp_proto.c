/*
 * wsp_proto.c — WSP/1 frame codec.
 *
 * Portable C99: no ESP-IDF, no POSIX, no malloc.  Compiled both into the
 * firmware and into the host unit tests (test/proto_test.c).
 */

#include "wsp_proto.h"

#include <string.h>

/* ------------------------------------------------------------------ */
/* CRC-16/CCITT-FALSE                                                 */
/* ------------------------------------------------------------------ */

static uint16_t crc16_update(uint16_t crc, const uint8_t *data, size_t len)
{
    for (size_t i = 0; i < len; i++) {
        crc ^= (uint16_t)((uint16_t)data[i] << 8);
        for (int bit = 0; bit < 8; bit++) {
            if (crc & 0x8000u) {
                crc = (uint16_t)((uint16_t)(crc << 1) ^ 0x1021u);
            } else {
                crc = (uint16_t)(crc << 1);
            }
        }
    }
    return crc;
}

uint16_t wsp_crc16(const uint8_t *data, size_t len)
{
    if (data == NULL && len != 0) {
        return 0;
    }
    return crc16_update(0xFFFFu, data, len);
}

/* ------------------------------------------------------------------ */
/* Encoder                                                            */
/* ------------------------------------------------------------------ */

size_t wsp_encode(uint8_t type, uint8_t flags, const uint8_t *payload,
                  uint16_t len, uint8_t *out, size_t out_cap)
{
    if (out == NULL) {
        return 0;
    }
    if (len > WSP_MAX_PAYLOAD) {
        return 0;
    }
    if (len != 0 && payload == NULL) {
        return 0;
    }

    const size_t total = (size_t)WSP_HEADER_LEN + (size_t)len;
    if (out_cap < total) {
        return 0;
    }

    out[0] = (uint8_t)WSP_MAGIC0;
    out[1] = (uint8_t)WSP_MAGIC1;
    out[2] = (uint8_t)(((uint16_t)WSP_VERSION << 4) | (uint16_t)(type & 0x0Fu));
    out[3] = flags;
    out[4] = (uint8_t)(len & 0xFFu);
    out[5] = (uint8_t)((len >> 8) & 0xFFu);

    if (len != 0) {
        memcpy(out + WSP_HEADER_LEN, payload, (size_t)len);
    }

    uint16_t crc = crc16_update(0xFFFFu, out, 6);
    if (len != 0) {
        crc = crc16_update(crc, out + WSP_HEADER_LEN, (size_t)len);
    }

    out[6] = (uint8_t)(crc & 0xFFu);
    out[7] = (uint8_t)((crc >> 8) & 0xFFu);

    return total;
}

/* ------------------------------------------------------------------ */
/* Streaming decoder                                                  */
/* ------------------------------------------------------------------ */

enum {
    ST_MAGIC0 = 0,
    ST_MAGIC1 = 1,
    ST_HEADER = 2,
    ST_PAYLOAD = 3,
};

void wsp_parser_init(wsp_parser_t *p)
{
    if (p == NULL) {
        return;
    }
    memset(p, 0, sizeof(*p));
    p->state = ST_MAGIC0;
}

/* Parse the completed 6-byte header.  Returns a WSP_E_* code. */
static int header_done(wsp_parser_t *p)
{
    p->type = (uint8_t)(p->header[2] & 0x0Fu);
    p->flags = p->header[3];
    p->len = (uint16_t)((uint16_t)p->header[4] | ((uint16_t)p->header[5] << 8));

    const uint8_t ver = (uint8_t)(p->header[2] >> 4);
    if (ver != WSP_VERSION) {
        p->stat_bad_version++;
        p->state = ST_MAGIC0;
        return WSP_E_VERSION;
    }
    if (p->len > WSP_MAX_PAYLOAD) {
        p->state = ST_MAGIC0;
        return WSP_E_LENGTH;
    }

    p->crc_calc = crc16_update(0xFFFFu, p->header, 6);
    p->payload_used = 0;

    if (p->len == 0) {
        p->state = ST_MAGIC0;
        /* CRC bytes follow immediately; handled by caller path below. */
        return WSP_OK;
    }

    p->state = ST_PAYLOAD;
    return WSP_OK;
}

/*
 * Implementation note: the CRC field itself (bytes 6 and 7) arrives between
 * the header and the payload, so the byte stream is:
 *
 *   [0..5] header | [6..7] crc | [8..8+len) payload
 *
 * The state machine therefore collects the header, then the two CRC bytes,
 * then the payload.
 */
enum {
    ST_CRC0 = 4,
    ST_CRC1 = 5,
};

int wsp_parser_feed(wsp_parser_t *p, const uint8_t *data, size_t len,
                    wsp_frame_cb cb, void *ctx)
{
    if (p == NULL || (data == NULL && len != 0)) {
        return WSP_E_NOMEM;
    }

    int last_err = WSP_OK;
    /* saved header-complete state, see below */
    for (size_t i = 0; i < len; i++) {
        const uint8_t b = data[i];

        switch (p->state) {
        case ST_MAGIC0:
            if (b == (uint8_t)WSP_MAGIC0) {
                p->header[0] = b;
                p->hdr_used = 1;
                p->state = ST_MAGIC1;
            } else {
                p->stat_resync++;
            }
            break;

        case ST_MAGIC1:
            if (b == (uint8_t)WSP_MAGIC1) {
                p->header[1] = b;
                p->hdr_used = 2;
                p->state = ST_HEADER;
            } else if (b == (uint8_t)WSP_MAGIC0) {
                /* possible restart: "W W S" */
                p->header[0] = b;
                p->hdr_used = 1;
                /* stay in ST_MAGIC1 */
            } else {
                p->stat_resync++;
                p->state = ST_MAGIC0;
            }
            break;

        case ST_HEADER:
            p->header[p->hdr_used++] = b;
            if (p->hdr_used == 6) {
                int rc = header_done(p);
                if (rc != WSP_OK) {
                    last_err = rc;
                    /* header_done already reset the state */
                } else {
                    p->state = ST_CRC0;
                }
            }
            break;

        case ST_CRC0:
            p->crc_wire = (uint16_t)b;
            p->state = ST_CRC1;
            break;

        case ST_CRC1:
            p->crc_wire = (uint16_t)(p->crc_wire | ((uint16_t)b << 8));
            if (p->len == 0) {
                if (p->crc_wire != p->crc_calc) {
                    p->stat_bad_crc++;
                    last_err = WSP_E_CRC;
                } else if (cb != NULL) {
                    cb(ctx, p->type, p->flags, p->payload, 0);
                }
                p->state = ST_MAGIC0;
            } else {
                p->state = ST_PAYLOAD;
            }
            break;

        case ST_PAYLOAD:
            p->payload[p->payload_used++] = b;
            if (p->payload_used == p->len) {
                p->crc_calc = crc16_update(p->crc_calc, p->payload, (size_t)p->len);
                if (p->crc_wire != p->crc_calc) {
                    p->stat_bad_crc++;
                    last_err = WSP_E_CRC;
                } else if (cb != NULL) {
                    cb(ctx, p->type, p->flags, p->payload, p->len);
                }
                p->state = ST_MAGIC0;
            }
            break;

        default:
            p->state = ST_MAGIC0;
            break;
        }
    }

    return last_err;
}
