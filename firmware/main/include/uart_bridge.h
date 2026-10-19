/*
 * uart_bridge.h — target UART1 bridge (RX/TX buffering, runtime reconfig,
 * DTR/RTS/BREAK line control, RESET_TARGET pulses and byte counters).
 */

#ifndef UART_BRIDGE_H
#define UART_BRIDGE_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"

typedef struct {
    uint32_t baud;      /* 50 .. 5000000 */
    uint8_t  data_bits; /* 5 .. 8        */
    uint8_t  parity;    /* 0 none, 1 odd, 2 even */
    uint8_t  stop_bits; /* 1 .. 2        */
    uint8_t  flow;      /* 0 none, 1 RTS/CTS */
} uart_cfg_t;

/* Check a format against the protocol range and the compiled-in pin mapping.
 * Returns false for out-of-range values or flow=1 without RTS/CTS pins. */
bool uart_bridge_cfg_valid(const uart_cfg_t *cfg);

/* Install UART1, start the RX pump task and apply *initial (invalid input
 * falls back to the Kconfig defaults). */
esp_err_t uart_bridge_init(const uart_cfg_t *initial);

/* Apply a new UART format.  Waits for the TX FIFO to drain and flushes the RX
 * buffers first, so no stale bytes leak into the new configuration.
 * On ESP_ERR_INVALID_ARG nothing is changed. */
esp_err_t uart_bridge_configure(const uart_cfg_t *req, uart_cfg_t *applied);

/* Snapshot the currently active format. */
void uart_bridge_get_cfg(uart_cfg_t *out);

/* Write bytes to the target, waiting at most timeout_ms for ring buffer space.
 * Returns ESP_ERR_TIMEOUT if the TX path stays blocked (e.g. CTS flow control)
 * for longer than the timeout. */
esp_err_t uart_bridge_write(const uint8_t *data, size_t len, uint32_t timeout_ms);

/* Read buffered target->host bytes.  Blocks up to timeout_ms.  Returns
 * ESP_ERR_TIMEOUT and *out_len == 0 when no data arrived. */
esp_err_t uart_bridge_read(uint8_t *buf, size_t cap, uint32_t timeout_ms,
                           size_t *out_len);

/* Drop both the driver RX FIFO and the stream buffer (used on session start
 * and after a UART reconfiguration). */
void uart_bridge_flush_input(void);

/* Drive the target control lines.  dtr/rts are host-side LINE_CTRL bit states
 * (asserted = true), brk holds the TX line low.  The resulting state bitmask
 * is stored in *state_out when non-NULL. */
esp_err_t uart_bridge_line_ctrl(bool dtr, bool rts, bool brk, uint8_t *state_out);

/* Current LINE_STATUS bitmask (WSP_ST_* bits). */
uint8_t uart_bridge_line_status(void);

/* RESET_TARGET: 0 = assert (low), 1 = release (high), 2 = pulse. */
esp_err_t uart_bridge_reset_target(uint8_t action, uint16_t pulse_ms);

/* Traffic counters, copied atomically. */
void uart_bridge_get_counters(uint32_t *rx_bytes, uint32_t *tx_bytes,
                              uint32_t *overflows);

#endif /* UART_BRIDGE_H */
