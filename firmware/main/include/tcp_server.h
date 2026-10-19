/*
 * tcp_server.h — WSP (3333) and RAW (3334) TCP transports.
 *
 * Only one serial client may own the UART at a time.  WSP, RAW and the HTTP
 * WebSocket terminal all compete for the same lock exposed here.
 */

#ifndef TCP_SERVER_H
#define TCP_SERVER_H

#include <stdbool.h>
#include <stdint.h>

#include "esp_err.h"

#include "wsp_proto.h"

typedef enum {
    TCP_CLIENT_NONE = 0,
    TCP_CLIENT_WSP,
    TCP_CLIENT_RAW,
    TCP_CLIENT_WS,
} tcp_client_kind_t;

/* Create both listening sockets and start the accept tasks. */
esp_err_t tcp_server_start(void);

/*
 * Take ownership of the UART for one client.
 * wait_ms == 0 means "do not wait"; UINT32_MAX means "wait forever".
 * Returns false when another client already owns the UART.
 */
bool tcp_server_acquire_uart(tcp_client_kind_t kind, uint32_t wait_ms);

/* Release the UART.  Must be called exactly once by the owner. */
void tcp_server_release_uart(tcp_client_kind_t kind);

bool tcp_server_client_connected(void);
tcp_client_kind_t tcp_server_client_kind(void);

/* WSP frames_in/frames_out/crc_errors plus the UART byte counters. */
void tcp_server_get_stats(wsp_stats_t *out);

#endif /* TCP_SERVER_H */
