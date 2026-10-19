/*
 * wsp_proto.h — Wireless Serial Protocol v1 (WSP/1)
 *
 * This header is the single source of truth shared by:
 *   - the ESP32-C3 firmware (firmware/main/wsp_proto.c)
 *   - the host-side test suite (test/proto_test.c, compiled with the host cc)
 *
 * It MUST stay free of any ESP-IDF / OS specific dependency so that it can be
 * compiled and unit-tested on a normal desktop machine.
 *
 * Wire format
 * -----------
 *   offset  size  field
 *   0       1     magic0  = 'W' (0x57)
 *   1       1     magic1  = 'S' (0x53)
 *   2       1     ver_type: high nibble = version(1), low nibble = type
 *   3       1     flags (reserved, must be 0)
 *   4       2     payload length, little endian (0 .. WSP_MAX_PAYLOAD)
 *   6       2     CRC-16/CCITT-FALSE (poly 0x1021, init 0xFFFF).  It is
 *                 computed over the 6 header bytes (magic0 .. len) followed
 *                 by the payload bytes; the CRC field itself is excluded.
 *   8       len   payload
 *
 *   All multi-byte integers are little endian.
 */

#ifndef WSP_PROTO_H
#define WSP_PROTO_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define WSP_MAGIC0         0x57u /* 'W' */
#define WSP_MAGIC1         0x53u /* 'S' */
#define WSP_VERSION        1u
#define WSP_MAX_PAYLOAD    1024u
#define WSP_HEADER_LEN     8u
#define WSP_MAX_FRAME      (WSP_HEADER_LEN + WSP_MAX_PAYLOAD)

/* Default TCP ports on the device. */
#define WSP_DEFAULT_WSP_PORT 3333u
#define WSP_DEFAULT_RAW_PORT 3334u
#define WSP_DEFAULT_HTTP_PORT 80u

/* ------------------------------------------------------------------ */
/* Frame types (low nibble of byte 2)                                 */
/* ------------------------------------------------------------------ */
#define WSP_T_HELLO        0x0u /* C->D  client greeting                       */
#define WSP_T_HELLO_ACK    0x1u /* D->C  wsp_dev_info_t                        */
#define WSP_T_DATA         0x2u /* both  raw UART bytes                        */
#define WSP_T_UART_CFG     0x3u /* C->D  wsp_uart_cfg_t                        */
#define WSP_T_UART_CFG_ACK 0x4u /* D->C  wsp_uart_cfg_t (applied)              */
#define WSP_T_LINE_CTRL    0x5u /* C->D  uint8 bitmask                         */
#define WSP_T_LINE_STATUS  0x6u /* D->C  wsp_line_status_t                     */
#define WSP_T_STATS        0x7u /* C->D(empty) / D->C wsp_stats_t              */
#define WSP_T_PING         0x8u /* both  opaque echo payload                   */
#define WSP_T_PONG         0x9u /* both  echo of a PING payload                */
#define WSP_T_ERROR        0xAu /* D->C  wsp_error_t                           */
#define WSP_T_RESET_TARGET 0xBu /* C->D  wsp_reset_cmd_t                       */
#define WSP_T_INFO         0xCu /* C->D(empty) / D->C wsp_dev_info_t mDNS-less */

/* ------------------------------------------------------------------ */
/* Plain old structs (packed, little endian on the wire)              */
/* ------------------------------------------------------------------ */

/* HELLO payload, sent by the host right after connecting. */
typedef struct __attribute__((packed)) {
    uint8_t  proto_ver;   /* must be WSP_VERSION */
    uint8_t  flags;       /* reserved, 0 */
    uint16_t max_payload; /* largest DATA payload the host accepts */
} wsp_hello_t;            /* 4 bytes */

/* HELLO_ACK / INFO payload, sent by the device. */
typedef struct __attribute__((packed)) {
    uint8_t  proto_ver;
    uint8_t  fw_major;
    uint8_t  fw_minor;
    uint8_t  fw_patch;
    uint32_t max_payload;
    uint32_t baud;
    uint8_t  data_bits;   /* 5..8   */
    uint8_t  parity;      /* 0 none, 1 odd, 2 even */
    uint8_t  stop_bits;   /* 1..2   */
    uint8_t  flow;        /* 0 none, 1 rts/cts */
    char     name[16];    /* NUL padded UTF-8 device name */
    uint8_t  mac[6];
    char     chip[8];     /* e.g. "esp32c3" */
    uint32_t uptime_s;
    uint16_t wsp_port;
    uint16_t raw_port;
    uint8_t  reserved[8];
} wsp_dev_info_t;         /* 62 bytes */

/* UART_CFG / UART_CFG_ACK payload. */
typedef struct __attribute__((packed)) {
    uint32_t baud;      /* 50 .. 5000000 */
    uint8_t  data_bits; /* 5..8 */
    uint8_t  parity;    /* 0 none, 1 odd, 2 even */
    uint8_t  stop_bits; /* 1..2 */
    uint8_t  flow;      /* 0 none, 1 rts/cts */
} wsp_uart_cfg_t;       /* 8 bytes */

/* LINE_CTRL bitmask, host -> device. */
#define WSP_LINE_DTR 0x01u
#define WSP_LINE_RTS 0x02u
#define WSP_LINE_BREAK 0x04u

/* LINE_STATUS payload, device -> host. */
typedef struct __attribute__((packed)) {
    uint8_t state;    /* WSP_LINE_* bits below (input lines as seen by device) */
    uint8_t reserved;
} wsp_line_status_t; /* 2 bytes */

/* LINE_STATUS state bits (device perspective). */
#define WSP_ST_DTR 0x01u /* host asserted DTR  -> mapped to a device GPIO (optional) */
#define WSP_ST_RTS 0x02u /* host asserted RTS  -> mapped to a device GPIO (optional) */
#define WSP_ST_CTS 0x04u /* CTS input level (flow control) */
#define WSP_ST_DSR 0x08u
#define WSP_ST_DCD 0x10u
#define WSP_ST_BREAK 0x20u

/* STATS payload. */
typedef struct __attribute__((packed)) {
    uint32_t uart_rx_bytes;  /* bytes received from the target hardware */
    uint32_t uart_tx_bytes;  /* bytes sent to the target hardware */
    uint32_t frames_in;
    uint32_t frames_out;
    uint32_t crc_errors;
    uint32_t uart_overflows; /* bytes dropped because the RX ring was full */
} wsp_stats_t;               /* 24 bytes */

/* ERROR payload: code byte followed by a human readable message. */
typedef struct __attribute__((packed)) {
    uint8_t code;
    char    msg[]; /* not NUL terminated necessarily */
} wsp_error_t;

#define WSP_ERR_NONE         0u
#define WSP_ERR_BAD_VERSION  1u
#define WSP_ERR_BAD_LENGTH   2u
#define WSP_ERR_BAD_CRC      3u
#define WSP_ERR_NOT_HELLO    4u
#define WSP_ERR_UNSUPPORTED  5u
#define WSP_ERR_INVALID_ARG  6u
#define WSP_ERR_BUSY         7u
#define WSP_ERR_INTERNAL     8u
#define WSP_ERR_UART_FAIL    9u

/* RESET_TARGET payload. */
typedef struct __attribute__((packed)) {
    uint8_t  action; /* 0 = assert reset, 1 = release reset, 2 = pulse */
    uint8_t  reserved;
    uint16_t pulse_ms; /* valid when action == 2 */
} wsp_reset_cmd_t;

/* ------------------------------------------------------------------ */
/* CRC                                                                */
/* ------------------------------------------------------------------ */

/* CRC-16/CCITT-FALSE: poly 0x1021, init 0xFFFF, no reflection, no xorout. */
uint16_t wsp_crc16(const uint8_t *data, size_t len);

/* ------------------------------------------------------------------ */
/* Encoder                                                            */
/* ------------------------------------------------------------------ */

/*
 * Encode one frame into `out`.
 * Returns the total frame size (WSP_HEADER_LEN + len) on success, 0 on error
 * (null pointers, len > WSP_MAX_PAYLOAD or out_cap < needed).
 */
size_t wsp_encode(uint8_t type, uint8_t flags, const uint8_t *payload, uint16_t len,
                  uint8_t *out, size_t out_cap);

/* ------------------------------------------------------------------ */
/* Streaming decoder                                                  */
/* ------------------------------------------------------------------ */

#define WSP_OK          0
#define WSP_E_MAGIC    -1 /* resynchronised after a garbage byte */
#define WSP_E_CRC      -2 /* frame dropped, parser resynchronised */
#define WSP_E_LENGTH   -3 /* declared length > WSP_MAX_PAYLOAD */
#define WSP_E_NOMEM    -4 /* parser not initialised / bad argument */
#define WSP_E_VERSION  -5 /* frame carried an unsupported protocol version */

typedef struct {
    uint8_t  state;
    uint8_t  type;
    uint8_t  flags;
    uint16_t len;
    uint16_t hdr_used;    /* bytes of the 6-byte pre-CRC header consumed */
    uint16_t payload_used;
    uint16_t crc_calc;
    uint16_t crc_wire;
    uint8_t  header[6];
    uint8_t  payload[WSP_MAX_PAYLOAD];
    uint32_t stat_bad_crc;     /* frames dropped because of a CRC error */
    uint32_t stat_bad_version; /* frames dropped because of a version mismatch */
    uint32_t stat_resync;      /* garbage bytes skipped while hunting magic */
} wsp_parser_t;

typedef void (*wsp_frame_cb)(void *ctx, uint8_t type, uint8_t flags,
                             const uint8_t *payload, uint16_t len);

/* Reset the parser to the idle state. */
void wsp_parser_init(wsp_parser_t *p);

/*
 * Feed `len` bytes into the parser.  All `len` bytes are always consumed.
 * Complete, CRC-valid frames are delivered through `cb` in order.
 * Returns WSP_OK, or the last recoverable WSP_E_* error encountered while
 * parsing this chunk (the offending frame is dropped and parsing continues).
 * Garbage bytes are skipped automatically and counted in stat_resync.
 */
int wsp_parser_feed(wsp_parser_t *p, const uint8_t *data, size_t len,
                    wsp_frame_cb cb, void *ctx);

#ifdef __cplusplus
}
#endif

#endif /* WSP_PROTO_H */
