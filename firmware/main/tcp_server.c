/*
 * tcp_server.c — WSP/1 framed transport (port 3333) and RAW byte stream
 * (port 3334).
 *
 * WSP session state machine:
 *
 *   accept -> [HELLO within 5 s] -> DATA state -> (UART <-> TCP until the
 *   client disconnects, an idle timeout fires or a fatal protocol error is
 *   seen) -> back to accept.
 *
 * Single client policy: WSP, RAW and the HTTP WebSocket terminal share one
 * UART mutex (tcp_server_acquire_uart).  A second client is rejected with
 * ERROR(BUSY) (WSP) or simply closed (RAW).
 */

#include "tcp_server.h"

#include <errno.h>
#include <inttypes.h>
#include <stdint.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

#include "esp_log.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"
#include "lwip/inet.h"
#include "netinet/in.h"
#include "netinet/tcp.h"
#include "sdkconfig.h"

#include "config_store.h"
#include "mdns_srv.h"
#include "uart_bridge.h"
#include "wifi_mgr.h"
#include "wsp_proto.h"

#define TCP_SERVER_TAG "tcp_server"

#define WSP_RX_CHUNK        2048
#define UART_CHUNK          1024
#define WSP_MAX_DRAIN_CHUNKS 16
#define SELECT_POLL_MS      20
#define SOCK_SEND_TIMEOUT_S 2
#define UART_WRITE_TIMEOUT_MS 1000

#define WSP_TASK_STACK 6144
#define RAW_TASK_STACK 5120
#define TCP_TASK_PRIO  5

/* The wire structs are shared with the host implementation; fail the build
 * early if the compiler lays them out differently. */
_Static_assert(sizeof(wsp_hello_t) == 4, "wsp_hello_t must be 4 bytes");
_Static_assert(sizeof(wsp_dev_info_t) == 62, "wsp_dev_info_t must be 62 bytes");
_Static_assert(sizeof(wsp_uart_cfg_t) == 8, "wsp_uart_cfg_t must be 8 bytes");
_Static_assert(sizeof(wsp_line_status_t) == 2, "wsp_line_status_t must be 2 bytes");
_Static_assert(sizeof(wsp_stats_t) == 24, "wsp_stats_t must be 24 bytes");
_Static_assert(sizeof(wsp_reset_cmd_t) == 4, "wsp_reset_cmd_t must be 4 bytes");

typedef struct {
    int fd;
    bool hello_ok;
    bool close_requested;
    TickType_t last_rx;
} wsp_conn_t;

static SemaphoreHandle_t s_client_lock;
static portMUX_TYPE s_state_mux = portMUX_INITIALIZER_UNLOCKED;
static tcp_client_kind_t s_client_kind;
static uint32_t s_frames_in;
static uint32_t s_frames_out;
static uint32_t s_crc_errors;
static int s_wsp_listen_fd = -1;
static int s_raw_listen_fd = -1;

/* Only one WSP session can exist at a time, so the ~1.1 KB parser and the
 * frame scratch buffer live in BSS instead of on the task stack. */
static wsp_parser_t s_parser;
static uint8_t s_tx_frame[WSP_MAX_FRAME];

/* ------------------------------------------------------------------ */
/* Small helpers                                                      */
/* ------------------------------------------------------------------ */

static esp_err_t configure_client_socket(int fd)
{
    int opt = 1;
    if (setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &opt, sizeof(opt)) != 0) {
        ESP_LOGW(TCP_SERVER_TAG, "TCP_NODELAY failed (errno %d)", errno);
    }
    (void)setsockopt(fd, SOL_SOCKET, SO_KEEPALIVE, &opt, sizeof(opt));
#if defined(TCP_KEEPIDLE) && defined(TCP_KEEPINTVL) && defined(TCP_KEEPCNT)
    const int idle = 60;
    const int intvl = 10;
    const int cnt = 3;
    (void)setsockopt(fd, IPPROTO_TCP, TCP_KEEPIDLE, &idle, sizeof(idle));
    (void)setsockopt(fd, IPPROTO_TCP, TCP_KEEPINTVL, &intvl, sizeof(intvl));
    (void)setsockopt(fd, IPPROTO_TCP, TCP_KEEPCNT, &cnt, sizeof(cnt));
#endif

    struct timeval tv;
    memset(&tv, 0, sizeof(tv));
    tv.tv_sec = SOCK_SEND_TIMEOUT_S;
    (void)setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv));

    tv.tv_sec = 0;
    tv.tv_usec = 100000;
    (void)setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
    return ESP_OK;
}

static int create_listen_socket(uint16_t port)
{
    const int fd = socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    if (fd < 0) {
        ESP_LOGE(TCP_SERVER_TAG, "socket() failed (errno %d)", errno);
        return -1;
    }

    int opt = 1;
    (void)setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &opt, sizeof(opt));

    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_addr.s_addr = htonl(INADDR_ANY);
    addr.sin_port = htons(port);

    if (bind(fd, (struct sockaddr *)&addr, sizeof(addr)) != 0) {
        ESP_LOGE(TCP_SERVER_TAG, "bind(port %u) failed (errno %d)", port, errno);
        (void)close(fd);
        return -1;
    }
    if (listen(fd, 2) != 0) {
        ESP_LOGE(TCP_SERVER_TAG, "listen(port %u) failed (errno %d)", port, errno);
        (void)close(fd);
        return -1;
    }
    return fd;
}

/* Blocking send of exactly len bytes; the socket carries SO_SNDTIMEO so a
 * stalled client cannot wedge the task forever. */
static int send_all(int fd, const uint8_t *data, size_t len)
{
    size_t off = 0;
    while (off < len) {
        const ssize_t n = send(fd, data + off, len - off, 0);
        if (n < 0) {
            if (errno == EINTR) {
                continue;
            }
            return -1;
        }
        if (n == 0) {
            return -1;
        }
        off += (size_t)n;
    }
    return 0;
}

static void close_client_socket(int fd)
{
    if (fd < 0) {
        return;
    }
    (void)shutdown(fd, SHUT_RDWR);
    (void)close(fd);
}

/* ------------------------------------------------------------------ */
/* Client ownership                                                   */
/* ------------------------------------------------------------------ */

bool tcp_server_acquire_uart(tcp_client_kind_t kind, uint32_t wait_ms)
{
    if (s_client_lock == NULL) {
        return false;
    }
    const TickType_t ticks = (wait_ms == UINT32_MAX) ? portMAX_DELAY
                                                     : pdMS_TO_TICKS(wait_ms);
    if (xSemaphoreTake(s_client_lock, ticks) != pdTRUE) {
        return false;
    }
    portENTER_CRITICAL(&s_state_mux);
    s_client_kind = kind;
    portEXIT_CRITICAL(&s_state_mux);
    return true;
}

void tcp_server_release_uart(tcp_client_kind_t kind)
{
    if (s_client_lock == NULL) {
        return;
    }
    portENTER_CRITICAL(&s_state_mux);
    if (s_client_kind == kind) {
        s_client_kind = TCP_CLIENT_NONE;
    }
    portEXIT_CRITICAL(&s_state_mux);
    (void)xSemaphoreGive(s_client_lock);
}

bool tcp_server_client_connected(void)
{
    portENTER_CRITICAL(&s_state_mux);
    const bool connected = (s_client_kind != TCP_CLIENT_NONE);
    portEXIT_CRITICAL(&s_state_mux);
    return connected;
}

tcp_client_kind_t tcp_server_client_kind(void)
{
    portENTER_CRITICAL(&s_state_mux);
    const tcp_client_kind_t kind = s_client_kind;
    portEXIT_CRITICAL(&s_state_mux);
    return kind;
}

void tcp_server_get_stats(wsp_stats_t *out)
{
    if (out == NULL) {
        return;
    }
    memset(out, 0, sizeof(*out));

    /* Copy through locals: the wire struct is packed and its members are not
     * guaranteed to be naturally aligned. */
    uint32_t rx_bytes = 0;
    uint32_t tx_bytes = 0;
    uint32_t overflows = 0;
    uart_bridge_get_counters(&rx_bytes, &tx_bytes, &overflows);
    out->uart_rx_bytes = rx_bytes;
    out->uart_tx_bytes = tx_bytes;
    out->uart_overflows = overflows;

    portENTER_CRITICAL(&s_state_mux);
    out->frames_in = s_frames_in;
    out->frames_out = s_frames_out;
    out->crc_errors = s_crc_errors;
    portEXIT_CRITICAL(&s_state_mux);
}

/* ------------------------------------------------------------------ */
/* WSP session                                                        */
/* ------------------------------------------------------------------ */

static void build_dev_info(wsp_dev_info_t *info)
{
    memset(info, 0, sizeof(*info));

    app_config_t cfg;
    config_store_get(&cfg);
    uart_cfg_t uart;
    uart_bridge_get_cfg(&uart);

    info->proto_ver = (uint8_t)WSP_VERSION;
    info->fw_major = (uint8_t)CONFIG_WSP_FW_VERSION_MAJOR;
    info->fw_minor = (uint8_t)CONFIG_WSP_FW_VERSION_MINOR;
    info->fw_patch = (uint8_t)CONFIG_WSP_FW_VERSION_PATCH;
    info->max_payload = WSP_MAX_PAYLOAD;
    info->baud = uart.baud;
    info->data_bits = uart.data_bits;
    info->parity = uart.parity;
    info->stop_bits = uart.stop_bits;
    info->flow = uart.flow;
    (void)strlcpy(info->name, cfg.name, sizeof(info->name));

    uint8_t mac[6] = {0};
    if (wifi_mgr_get_mac(mac) == ESP_OK) {
        memcpy(info->mac, mac, sizeof(info->mac));
    }
    (void)strlcpy(info->chip, "esp32c3", sizeof(info->chip));
    info->uptime_s = (uint32_t)(esp_timer_get_time() / 1000000LL);
    info->wsp_port = cfg.wsp_port;
    info->raw_port = cfg.raw_port;
}

static int wsp_send(wsp_conn_t *conn, uint8_t type, const uint8_t *payload,
                    uint16_t len)
{
    if (conn->close_requested) {
        return -1;
    }
    const size_t frame_len = wsp_encode(type, 0, payload, len, s_tx_frame,
                                        sizeof(s_tx_frame));
    if (frame_len == 0) {
        ESP_LOGE(TCP_SERVER_TAG, "cannot encode type 0x%X frame (len %u)",
                 (unsigned)type, (unsigned)len);
        conn->close_requested = true;
        return -1;
    }
    if (send_all(conn->fd, s_tx_frame, frame_len) != 0) {
        conn->close_requested = true;
        return -1;
    }
    portENTER_CRITICAL(&s_state_mux);
    s_frames_out++;
    portEXIT_CRITICAL(&s_state_mux);
    return 0;
}

static void wsp_send_error(wsp_conn_t *conn, uint8_t code, const char *message)
{
    uint8_t payload[1 + 96];
    size_t msg_len = (message != NULL) ? strlen(message) : 0;
    if (msg_len > sizeof(payload) - 1) {
        msg_len = sizeof(payload) - 1;
    }
    payload[0] = code;
    if (msg_len > 0) {
        memcpy(&payload[1], message, msg_len);
    }
    (void)wsp_send(conn, WSP_T_ERROR, payload, (uint16_t)(1 + msg_len));
}

static void wsp_send_dev_info(wsp_conn_t *conn, uint8_t type)
{
    wsp_dev_info_t info;
    build_dev_info(&info);
    if (wsp_send(conn, type, (const uint8_t *)&info, (uint16_t)sizeof(info)) != 0) {
        ESP_LOGW(TCP_SERVER_TAG, "failed to send device info");
    }
}

static void wsp_handle_uart_cfg(wsp_conn_t *conn, const uint8_t *payload,
                                uint16_t len)
{
    if (len < sizeof(wsp_uart_cfg_t)) {
        wsp_send_error(conn, WSP_ERR_BAD_LENGTH, "UART_CFG payload too short");
        return;
    }
    wsp_uart_cfg_t req;
    memcpy(&req, payload, sizeof(req));

    const uart_cfg_t want = {
        .baud = req.baud,
        .data_bits = req.data_bits,
        .parity = req.parity,
        .stop_bits = req.stop_bits,
        .flow = req.flow,
    };
    uart_cfg_t applied;
    memset(&applied, 0, sizeof(applied));
    esp_err_t err = uart_bridge_configure(&want, &applied);
    if (err != ESP_OK) {
        ESP_LOGW(TCP_SERVER_TAG, "UART_CFG rejected (baud=%" PRIu32 ")",
                 req.baud);
        wsp_send_error(conn, (err == ESP_ERR_INVALID_ARG) ? WSP_ERR_INVALID_ARG
                                                          : WSP_ERR_UART_FAIL,
                       "cannot apply UART configuration");
        return;
    }

    err = config_store_set_uart(applied.baud, applied.data_bits, applied.parity,
                                applied.stop_bits, applied.flow);
    if (err != ESP_OK) {
        ESP_LOGW(TCP_SERVER_TAG, "cannot persist UART config: %s",
                 esp_err_to_name(err));
    }
    (void)mdns_srv_refresh();

    const wsp_uart_cfg_t ack = {
        .baud = applied.baud,
        .data_bits = applied.data_bits,
        .parity = applied.parity,
        .stop_bits = applied.stop_bits,
        .flow = applied.flow,
    };
    (void)wsp_send(conn, WSP_T_UART_CFG_ACK, (const uint8_t *)&ack,
                   (uint16_t)sizeof(ack));
    ESP_LOGI(TCP_SERVER_TAG, "UART_CFG applied: %" PRIu32 " %u%c%u flow=%u",
             applied.baud, applied.data_bits,
             (applied.parity == 1u) ? 'O' : ((applied.parity == 2u) ? 'E' : 'N'),
             applied.stop_bits, applied.flow);
}

static void wsp_handle_line_ctrl(wsp_conn_t *conn, const uint8_t *payload,
                                 uint16_t len)
{
    if (len < 1) {
        wsp_send_error(conn, WSP_ERR_BAD_LENGTH, "LINE_CTRL payload too short");
        return;
    }
    const uint8_t mask = payload[0];
    uint8_t state = 0;
    const esp_err_t err = uart_bridge_line_ctrl(
        (mask & WSP_LINE_DTR) != 0, (mask & WSP_LINE_RTS) != 0,
        (mask & WSP_LINE_BREAK) != 0, &state);
    if (err != ESP_OK) {
        wsp_send_error(conn, WSP_ERR_INTERNAL, "line control failed");
        return;
    }
    const wsp_line_status_t status = {
        .state = state,
        .reserved = 0,
    };
    (void)wsp_send(conn, WSP_T_LINE_STATUS, (const uint8_t *)&status,
                   (uint16_t)sizeof(status));
}

static void wsp_handle_reset_target(wsp_conn_t *conn, const uint8_t *payload,
                                    uint16_t len)
{
    if (len < sizeof(wsp_reset_cmd_t)) {
        wsp_send_error(conn, WSP_ERR_BAD_LENGTH, "RESET_TARGET payload too short");
        return;
    }
    wsp_reset_cmd_t cmd;
    memcpy(&cmd, payload, sizeof(cmd));

    esp_err_t err;
    switch (cmd.action) {
    case 0u:
    case 1u:
        err = uart_bridge_reset_target(cmd.action, 0);
        break;
    case 2u:
        err = uart_bridge_reset_target(cmd.action, cmd.pulse_ms);
        break;
    default:
        err = ESP_ERR_INVALID_ARG;
        break;
    }

    if (err == ESP_ERR_NOT_SUPPORTED) {
        wsp_send_error(conn, WSP_ERR_UNSUPPORTED, "reset GPIO not configured");
    } else if (err != ESP_OK) {
        wsp_send_error(conn, (err == ESP_ERR_INVALID_ARG) ? WSP_ERR_INVALID_ARG
                                                          : WSP_ERR_INTERNAL,
                       "invalid RESET_TARGET request");
    }
}

static void wsp_on_frame(void *ctx, uint8_t type, uint8_t flags,
                         const uint8_t *payload, uint16_t len)
{
    wsp_conn_t *conn = (wsp_conn_t *)ctx;
    (void)flags;

    if (conn->close_requested) {
        return;
    }

    portENTER_CRITICAL(&s_state_mux);
    s_frames_in++;
    portEXIT_CRITICAL(&s_state_mux);

    if (!conn->hello_ok) {
        if (type != WSP_T_HELLO) {
            wsp_send_error(conn, WSP_ERR_NOT_HELLO, "HELLO required first");
            conn->close_requested = true;
            return;
        }
        if (len < sizeof(wsp_hello_t)) {
            wsp_send_error(conn, WSP_ERR_BAD_LENGTH, "HELLO payload too short");
            conn->close_requested = true;
            return;
        }
        wsp_hello_t hello;
        memcpy(&hello, payload, sizeof(hello));
        if (hello.proto_ver != (uint8_t)WSP_VERSION) {
            wsp_send_error(conn, WSP_ERR_BAD_VERSION, "unsupported protocol version");
            conn->close_requested = true;
            return;
        }
        conn->hello_ok = true;
        ESP_LOGI(TCP_SERVER_TAG, "WSP handshake done (client max_payload=%u)",
                 hello.max_payload);
        wsp_send_dev_info(conn, WSP_T_HELLO_ACK);
        return;
    }

    switch (type) {
    case WSP_T_DATA: {
        if (len == 0) {
            wsp_send_error(conn, WSP_ERR_BAD_LENGTH, "empty DATA frame");
            break;
        }
        const esp_err_t err = uart_bridge_write(payload, len,
                                                UART_WRITE_TIMEOUT_MS);
        if (err != ESP_OK) {
            ESP_LOGW(TCP_SERVER_TAG, "UART write of %u bytes failed: %s",
                     (unsigned)len, esp_err_to_name(err));
            wsp_send_error(conn, WSP_ERR_UART_FAIL, "UART write failed");
        }
        break;
    }
    case WSP_T_UART_CFG:
        wsp_handle_uart_cfg(conn, payload, len);
        break;
    case WSP_T_LINE_CTRL:
        wsp_handle_line_ctrl(conn, payload, len);
        break;
    case WSP_T_STATS: {
        wsp_stats_t stats;
        tcp_server_get_stats(&stats);
        (void)wsp_send(conn, WSP_T_STATS, (const uint8_t *)&stats,
                       (uint16_t)sizeof(stats));
        break;
    }
    case WSP_T_PING: {
        if (len > 64) {
            wsp_send_error(conn, WSP_ERR_BAD_LENGTH, "PING payload too long");
            break;
        }
        (void)wsp_send(conn, WSP_T_PONG, payload, len);
        break;
    }
    case WSP_T_RESET_TARGET:
        wsp_handle_reset_target(conn, payload, len);
        break;
    case WSP_T_INFO:
        wsp_send_dev_info(conn, WSP_T_INFO);
        break;
    case WSP_T_PONG:       /* client answer to our PING: nothing to do */
    case WSP_T_UART_CFG_ACK:
    case WSP_T_HELLO_ACK:
    case WSP_T_LINE_STATUS:
    case WSP_T_ERROR:
        break;
    case WSP_T_HELLO:
        /* Duplicate HELLO in DATA state: tolerate and ignore. */
        break;
    default:
        ESP_LOGW(TCP_SERVER_TAG, "unsupported WSP frame type 0x%X", (unsigned)type);
        wsp_send_error(conn, WSP_ERR_UNSUPPORTED, "unsupported frame type");
        break;
    }
}

static void wsp_session(int fd)
{
    wsp_conn_t conn;
    memset(&conn, 0, sizeof(conn));
    conn.fd = fd;
    conn.last_rx = xTaskGetTickCount();

    const TickType_t hello_timeout = pdMS_TO_TICKS(CONFIG_WSP_HELLO_TIMEOUT_MS);
    const TickType_t idle_timeout = pdMS_TO_TICKS((uint32_t)CONFIG_WSP_IDLE_TIMEOUT_S * 1000u);
    const TickType_t handshake_deadline = conn.last_rx + hello_timeout;

    (void)configure_client_socket(fd);
    wsp_parser_init(&s_parser);
    uart_bridge_flush_input();
    (void)uart_bridge_line_ctrl(false, false, false, NULL);

    ESP_LOGI(TCP_SERVER_TAG, "WSP client attached to UART");

    uint8_t rx_buf[WSP_RX_CHUNK];
    uint8_t uart_buf[UART_CHUNK];
    uint32_t crc_seen = s_parser.stat_bad_crc;

    while (!conn.close_requested) {
        fd_set read_fds;
        FD_ZERO(&read_fds);
        FD_SET(fd, &read_fds);
        struct timeval tv;
        tv.tv_sec = 0;
        tv.tv_usec = SELECT_POLL_MS * 1000;

        const int ready = select(fd + 1, &read_fds, NULL, NULL, &tv);
        if (ready < 0) {
            if (errno == EINTR) {
                continue;
            }
            break;
        }

        if (ready > 0 && FD_ISSET(fd, &read_fds)) {
            const ssize_t n = recv(fd, rx_buf, sizeof(rx_buf), 0);
            if (n == 0) {
                ESP_LOGI(TCP_SERVER_TAG, "WSP client closed the connection");
                break;
            }
            if (n < 0) {
                if (errno == EINTR) {
                    continue;
                }
                if (errno != EAGAIN && errno != EWOULDBLOCK) {
                    ESP_LOGW(TCP_SERVER_TAG, "recv failed (errno %d)", errno);
                    break;
                }
            } else {
                conn.last_rx = xTaskGetTickCount();
                const int rc = wsp_parser_feed(&s_parser, rx_buf, (size_t)n,
                                               wsp_on_frame, &conn);
                const uint32_t crc_now = s_parser.stat_bad_crc;
                if (crc_now != crc_seen) {
                    portENTER_CRITICAL(&s_state_mux);
                    s_crc_errors += crc_now - crc_seen;
                    portEXIT_CRITICAL(&s_state_mux);
                    crc_seen = crc_now;
                }
                if (rc == WSP_E_VERSION) {
                    wsp_send_error(&conn, WSP_ERR_BAD_VERSION,
                                   "unsupported protocol version");
                    break;
                }
                if (rc == WSP_E_CRC) {
                    wsp_send_error(&conn, WSP_ERR_BAD_CRC, "frame CRC mismatch");
                } else if (rc == WSP_E_LENGTH) {
                    wsp_send_error(&conn, WSP_ERR_BAD_LENGTH, "frame too long");
                } else if (rc == WSP_E_NOMEM) {
                    break;
                }
            }
        }

        if (!conn.hello_ok) {
            if (xTaskGetTickCount() > handshake_deadline) {
                ESP_LOGW(TCP_SERVER_TAG, "no HELLO within %d ms, closing",
                         CONFIG_WSP_HELLO_TIMEOUT_MS);
                break;
            }
            continue;
        }

        for (int i = 0; i < WSP_MAX_DRAIN_CHUNKS && !conn.close_requested; i++) {
            size_t got = 0;
            const esp_err_t err = uart_bridge_read(uart_buf, sizeof(uart_buf), 0,
                                                   &got);
            if (err != ESP_OK || got == 0) {
                break;
            }
            if (wsp_send(&conn, WSP_T_DATA, uart_buf, (uint16_t)got) != 0) {
                break;
            }
            if (got < sizeof(uart_buf)) {
                break;
            }
        }

        if (xTaskGetTickCount() - conn.last_rx > idle_timeout) {
            ESP_LOGW(TCP_SERVER_TAG, "WSP idle timeout (%d s), closing",
                     CONFIG_WSP_IDLE_TIMEOUT_S);
            break;
        }
    }

    /* Do not leave the target held in reset after the client vanishes. */
    (void)uart_bridge_line_ctrl(false, false, false, NULL);
    uart_bridge_flush_input();
    close_client_socket(fd);
    ESP_LOGI(TCP_SERVER_TAG, "WSP client detached from UART");
    tcp_server_release_uart(TCP_CLIENT_WSP);
}

static void wsp_accept_task(void *arg)
{
    (void)arg;

    for (;;) {
        struct sockaddr_in peer;
        socklen_t peer_len = sizeof(peer);
        const int fd = accept(s_wsp_listen_fd, (struct sockaddr *)&peer, &peer_len);
        if (fd < 0) {
            if (errno == EINTR) {
                continue;
            }
            ESP_LOGW(TCP_SERVER_TAG, "WSP accept failed (errno %d)", errno);
            vTaskDelay(pdMS_TO_TICKS(100));
            continue;
        }

        char peer_ip[16] = {0};
        (void)inet_ntoa_r(peer.sin_addr, peer_ip, sizeof(peer_ip));
        ESP_LOGI(TCP_SERVER_TAG, "WSP connection from %s", peer_ip);
        (void)configure_client_socket(fd);

        if (!tcp_server_acquire_uart(TCP_CLIENT_WSP, 0)) {
            ESP_LOGW(TCP_SERVER_TAG, "UART busy, rejecting WSP client");
            uint8_t payload[5] = { WSP_ERR_BUSY, 'b', 'u', 's', 'y' };
            uint8_t frame[WSP_HEADER_LEN + sizeof(payload)];
            const size_t frame_len = wsp_encode(WSP_T_ERROR, 0, payload,
                                                (uint16_t)sizeof(payload),
                                                frame, sizeof(frame));
            if (frame_len > 0) {
                (void)send_all(fd, frame, frame_len);
            }
            close_client_socket(fd);
            continue;
        }

        wsp_session(fd);
    }
}

/* ------------------------------------------------------------------ */
/* RAW session                                                        */
/* ------------------------------------------------------------------ */

static void raw_session(int fd)
{
    (void)configure_client_socket(fd);
    uart_bridge_flush_input();
    ESP_LOGI(TCP_SERVER_TAG, "RAW client attached to UART");

    uint8_t rx_buf[UART_CHUNK];

    for (;;) {
        fd_set read_fds;
        FD_ZERO(&read_fds);
        FD_SET(fd, &read_fds);
        struct timeval tv;
        tv.tv_sec = 0;
        tv.tv_usec = SELECT_POLL_MS * 1000;

        const int ready = select(fd + 1, &read_fds, NULL, NULL, &tv);
        if (ready < 0) {
            if (errno == EINTR) {
                continue;
            }
            break;
        }

        if (ready > 0 && FD_ISSET(fd, &read_fds)) {
            const ssize_t n = recv(fd, rx_buf, sizeof(rx_buf), 0);
            if (n == 0) {
                break;
            }
            if (n < 0) {
                if (errno == EINTR) {
                    continue;
                }
                if (errno != EAGAIN && errno != EWOULDBLOCK) {
                    break;
                }
            } else {
                const esp_err_t err = uart_bridge_write(rx_buf, (size_t)n,
                                                        UART_WRITE_TIMEOUT_MS);
                if (err != ESP_OK) {
                    ESP_LOGW(TCP_SERVER_TAG, "RAW->UART write failed: %s",
                             esp_err_to_name(err));
                }
            }
        }

        for (int i = 0; i < WSP_MAX_DRAIN_CHUNKS; i++) {
            size_t got = 0;
            const esp_err_t err = uart_bridge_read(rx_buf, sizeof(rx_buf), 0, &got);
            if (err != ESP_OK || got == 0) {
                break;
            }
            if (send_all(fd, rx_buf, got) != 0) {
                goto done;
            }
            if (got < sizeof(rx_buf)) {
                break;
            }
        }
    }

done:
    uart_bridge_flush_input();
    close_client_socket(fd);
    ESP_LOGI(TCP_SERVER_TAG, "RAW client detached from UART");
    tcp_server_release_uart(TCP_CLIENT_RAW);
}

static void raw_accept_task(void *arg)
{
    (void)arg;

    for (;;) {
        struct sockaddr_in peer;
        socklen_t peer_len = sizeof(peer);
        const int fd = accept(s_raw_listen_fd, (struct sockaddr *)&peer, &peer_len);
        if (fd < 0) {
            if (errno == EINTR) {
                continue;
            }
            ESP_LOGW(TCP_SERVER_TAG, "RAW accept failed (errno %d)", errno);
            vTaskDelay(pdMS_TO_TICKS(100));
            continue;
        }

        char peer_ip[16] = {0};
        (void)inet_ntoa_r(peer.sin_addr, peer_ip, sizeof(peer_ip));
        ESP_LOGI(TCP_SERVER_TAG, "RAW connection from %s", peer_ip);

        if (!tcp_server_acquire_uart(TCP_CLIENT_RAW, 0)) {
            ESP_LOGW(TCP_SERVER_TAG, "UART busy, rejecting RAW client");
            close_client_socket(fd);
            continue;
        }

        raw_session(fd);
    }
}

/* ------------------------------------------------------------------ */
/* Start                                                              */
/* ------------------------------------------------------------------ */

esp_err_t tcp_server_start(void)
{
    if (s_client_lock == NULL) {
        s_client_lock = xSemaphoreCreateMutex();
        if (s_client_lock == NULL) {
            ESP_LOGE(TCP_SERVER_TAG, "cannot create UART ownership mutex");
            return ESP_ERR_NO_MEM;
        }
    }

    app_config_t cfg;
    config_store_get(&cfg);

    s_wsp_listen_fd = create_listen_socket(cfg.wsp_port);
    if (s_wsp_listen_fd < 0) {
        return ESP_FAIL;
    }
    s_raw_listen_fd = create_listen_socket(cfg.raw_port);
    if (s_raw_listen_fd < 0) {
        (void)close(s_wsp_listen_fd);
        s_wsp_listen_fd = -1;
        return ESP_FAIL;
    }

    BaseType_t ok = xTaskCreate(wsp_accept_task, "wsp_accept", WSP_TASK_STACK, NULL,
                                TCP_TASK_PRIO, NULL);
    if (ok != pdPASS) {
        ESP_LOGE(TCP_SERVER_TAG, "cannot create WSP accept task");
        return ESP_ERR_NO_MEM;
    }
    ok = xTaskCreate(raw_accept_task, "raw_accept", RAW_TASK_STACK, NULL,
                     TCP_TASK_PRIO, NULL);
    if (ok != pdPASS) {
        ESP_LOGE(TCP_SERVER_TAG, "cannot create RAW accept task");
        return ESP_ERR_NO_MEM;
    }

    ESP_LOGI(TCP_SERVER_TAG, "listening: WSP=%u RAW=%u", cfg.wsp_port, cfg.raw_port);
    return ESP_OK;
}
