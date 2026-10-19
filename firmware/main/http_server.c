/*
 * http_server.c — device web UI and JSON API.
 *
 *   GET  /               embedded single page app (config + serial terminal)
 *   GET  /api/status     device / UART / traffic status
 *   GET  /api/config     persisted configuration (no WiFi password)
 *   POST /api/config     partial configuration update
 *   GET  /api/scan       neighbour WiFi scan
 *   POST /api/reboot     restart the device
 *   WS   /ws             binary frames = raw UART data,
 *                        text frames   = JSON control commands
 *
 * The WebSocket terminal takes exclusive ownership of the UART, exactly like a
 * WSP or RAW client, and publishes incoming UART bytes as binary WS frames
 * from a dedicated pump task.
 */

#include "http_server.h"

#include <inttypes.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <netinet/tcp.h>

#include "cJSON.h"
#include "esp_http_server.h"
#include "esp_log.h"
#include "esp_system.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"
#include "sdkconfig.h"

#include "config_store.h"
#include "mdns_srv.h"
#include "tcp_server.h"
#include "uart_bridge.h"
#include "wifi_mgr.h"
#include "wsp_proto.h"

#define HTTP_SERVER_TAG "http_server"

#define WS_PUMP_TASK_STACK 4096
#define WS_PUMP_TASK_PRIO  6
#define WS_MAX_TEXT_FRAME  1024
#define MAX_SCAN_RESULTS   20
#define CONFIG_BODY_MAX    1024

/* Embedded by EMBED_TXTFILES (NUL terminated). */
extern const uint8_t index_html_start[] asm("_binary_index_html_start");

static httpd_handle_t s_server;
static SemaphoreHandle_t s_ws_tx_lock;
static struct {
    httpd_handle_t hd;
    int fd;
    TaskHandle_t task;
} s_ws;

/* ------------------------------------------------------------------ */
/* Small helpers                                                      */
/* ------------------------------------------------------------------ */

static const char *mode_name(wifi_mgr_mode_t mode)
{
    switch (mode) {
    case WIFI_MGR_MODE_STA: return "sta";
    case WIFI_MGR_MODE_AP:  return "ap";
    default:                return "none";
    }
}

static const char *parity_name(uint8_t parity)
{
    switch (parity) {
    case 1: return "odd";
    case 2: return "even";
    default: return "none";
    }
}

static const char *client_kind_name(tcp_client_kind_t kind)
{
    switch (kind) {
    case TCP_CLIENT_WSP: return "wsp";
    case TCP_CLIENT_RAW: return "raw";
    case TCP_CLIENT_WS:  return "ws";
    default:             return "none";
    }
}

static const char *auth_name(wifi_auth_mode_t mode)
{
    switch (mode) {
    case WIFI_AUTH_OPEN:            return "open";
    case WIFI_AUTH_WEP:             return "wep";
    case WIFI_AUTH_WPA_PSK:         return "wpa";
    case WIFI_AUTH_WPA2_PSK:        return "wpa2";
    case WIFI_AUTH_WPA_WPA2_PSK:    return "wpa/wpa2";
    case WIFI_AUTH_WPA3_PSK:        return "wpa3";
    case WIFI_AUTH_WPA2_WPA3_PSK:   return "wpa2/wpa3";
    default:                        return "unknown";
    }
}

static esp_err_t send_json(httpd_req_t *req, cJSON *root)
{
    if (root == NULL) {
        return httpd_resp_send_500(req);
    }
    char *text = cJSON_PrintUnformatted(root);
    if (text == NULL) {
        return httpd_resp_send_500(req);
    }
    httpd_resp_set_type(req, "application/json");
    httpd_resp_set_hdr(req, "Cache-Control", "no-store");
    const esp_err_t err = httpd_resp_send(req, text, HTTPD_RESP_USE_STRLEN);
    cJSON_free(text);
    return err;
}

static esp_err_t send_error_json(httpd_req_t *req, const char *status,
                                 const char *message)
{
    cJSON *root = cJSON_CreateObject();
    if (root != NULL) {
        cJSON_AddBoolToObject(root, "ok", false);
        cJSON_AddStringToObject(root, "error", message);
    }
    httpd_resp_set_status(req, status);
    const esp_err_t err = send_json(req, root);
    cJSON_Delete(root);
    return err;
}

static esp_err_t read_request_body(httpd_req_t *req, char *buf, size_t cap,
                                   size_t *out_len)
{
    if (out_len == NULL) {
        return ESP_ERR_INVALID_ARG;
    }
    *out_len = 0;
    if (req->content_len == 0) {
        buf[0] = '\0';
        return ESP_OK;
    }
    if (req->content_len >= cap) {
        return ESP_ERR_INVALID_SIZE;
    }

    size_t received = 0;
    while (received < req->content_len) {
        const int n = httpd_req_recv(req, buf + received,
                                     req->content_len - received);
        if (n == HTTPD_SOCK_ERR_TIMEOUT) {
            continue;
        }
        if (n <= 0) {
            return ESP_FAIL;
        }
        received += (size_t)n;
    }
    buf[received] = '\0';
    *out_len = received;
    return ESP_OK;
}

/* ------------------------------------------------------------------ */
/* JSON builders                                                      */
/* ------------------------------------------------------------------ */

static cJSON *build_uart_json(void)
{
    uart_cfg_t uart;
    uart_bridge_get_cfg(&uart);

    cJSON *root = cJSON_CreateObject();
    if (root == NULL) {
        return NULL;
    }
    cJSON_AddNumberToObject(root, "baud", (double)uart.baud);
    cJSON_AddNumberToObject(root, "data_bits", uart.data_bits);
    cJSON_AddStringToObject(root, "parity", parity_name(uart.parity));
    cJSON_AddNumberToObject(root, "stop_bits", uart.stop_bits);
    cJSON_AddNumberToObject(root, "flow", uart.flow);
    return root;
}

static cJSON *build_status_json(void)
{
    app_config_t cfg;
    config_store_get(&cfg);
    wifi_mgr_status_t wifi;
    wifi_mgr_get_status(&wifi);
    wsp_stats_t stats;
    tcp_server_get_stats(&stats);

    cJSON *root = cJSON_CreateObject();
    if (root == NULL) {
        return NULL;
    }

    cJSON_AddStringToObject(root, "name", cfg.name);
    cJSON_AddStringToObject(root, "chip", "esp32c3");

    uint8_t mac[6] = {0};
    char mac_str[18] = "--";
    if (wifi_mgr_get_mac(mac) == ESP_OK) {
        (void)snprintf(mac_str, sizeof(mac_str), "%02x:%02x:%02x:%02x:%02x:%02x",
                       mac[0], mac[1], mac[2], mac[3], mac[4], mac[5]);
    }
    cJSON_AddStringToObject(root, "mac", mac_str);
    cJSON_AddStringToObject(root, "ip", wifi.ip);
    cJSON_AddStringToObject(root, "wifi_mode", mode_name(wifi.mode));
    cJSON_AddStringToObject(root, "wifi_ssid", wifi.ssid);
    cJSON_AddNumberToObject(root, "rssi", wifi.rssi);
    cJSON_AddNumberToObject(root, "uptime",
                            (double)(esp_timer_get_time() / 1000000LL));
    cJSON_AddNumberToObject(root, "free_heap", (double)esp_get_free_heap_size());
    cJSON_AddNumberToObject(root, "wsp_port", cfg.wsp_port);
    cJSON_AddNumberToObject(root, "raw_port", cfg.raw_port);
    cJSON_AddBoolToObject(root, "client_connected",
                          tcp_server_client_connected());
    cJSON_AddStringToObject(root, "client",
                            client_kind_name(tcp_server_client_kind()));

    cJSON_AddItemToObject(root, "uart", build_uart_json());

    cJSON *st = cJSON_AddObjectToObject(root, "stats");
    if (st != NULL) {
        cJSON_AddNumberToObject(st, "uart_rx_bytes", (double)stats.uart_rx_bytes);
        cJSON_AddNumberToObject(st, "uart_tx_bytes", (double)stats.uart_tx_bytes);
        cJSON_AddNumberToObject(st, "frames_in", (double)stats.frames_in);
        cJSON_AddNumberToObject(st, "frames_out", (double)stats.frames_out);
        cJSON_AddNumberToObject(st, "crc_errors", (double)stats.crc_errors);
        cJSON_AddNumberToObject(st, "uart_overflows",
                                (double)stats.uart_overflows);
    }
    return root;
}

static cJSON *build_config_json(void)
{
    app_config_t cfg;
    config_store_get(&cfg);

    cJSON *root = cJSON_CreateObject();
    if (root == NULL) {
        return NULL;
    }
    cJSON_AddStringToObject(root, "name", cfg.name);
    cJSON_AddNumberToObject(root, "baud", (double)cfg.baud);
    cJSON_AddNumberToObject(root, "data_bits", cfg.data_bits);
    cJSON_AddStringToObject(root, "parity", parity_name(cfg.parity));
    cJSON_AddNumberToObject(root, "stop_bits", cfg.stop_bits);
    cJSON_AddNumberToObject(root, "flow", cfg.flow);
    cJSON_AddNumberToObject(root, "wsp_port", cfg.wsp_port);
    cJSON_AddNumberToObject(root, "raw_port", cfg.raw_port);

    cJSON *wifi = cJSON_AddObjectToObject(root, "wifi");
    if (wifi != NULL) {
        cJSON_AddStringToObject(wifi, "ssid", cfg.wifi_ssid);
        cJSON_AddBoolToObject(wifi, "set", cfg.wifi_configured);
    }
    return root;
}

/* ------------------------------------------------------------------ */
/* JSON parsing helpers                                               */
/* ------------------------------------------------------------------ */

static bool json_get_u32(const cJSON *obj, const char *name, uint32_t *out)
{
    const cJSON *item = cJSON_GetObjectItemCaseSensitive(obj, name);
    if (!cJSON_IsNumber(item) || item->valuedouble < 0) {
        return false;
    }
    *out = (uint32_t)item->valuedouble;
    return true;
}

static bool json_get_bool(const cJSON *obj, const char *name, bool *out)
{
    const cJSON *item = cJSON_GetObjectItemCaseSensitive(obj, name);
    if (item == NULL) {
        return false;
    }
    if (cJSON_IsBool(item)) {
        *out = cJSON_IsTrue(item);
        return true;
    }
    if (cJSON_IsNumber(item)) {
        *out = (item->valueint != 0);
        return true;
    }
    return false;
}

static bool json_get_string(const cJSON *obj, const char *name,
                            const char **out)
{
    const cJSON *item = cJSON_GetObjectItemCaseSensitive(obj, name);
    if (!cJSON_IsString(item)) {
        return false;
    }
    *out = item->valuestring;
    return true;
}

static bool json_parse_parity(const cJSON *obj, uint8_t *out)
{
    const cJSON *item = cJSON_GetObjectItemCaseSensitive(obj, "parity");
    if (item == NULL) {
        return false;
    }
    if (cJSON_IsNumber(item)) {
        if (item->valueint < 0 || item->valueint > 2) {
            return false;
        }
        *out = (uint8_t)item->valueint;
        return true;
    }
    if (cJSON_IsString(item)) {
        if (strcmp(item->valuestring, "none") == 0) {
            *out = 0;
            return true;
        }
        if (strcmp(item->valuestring, "odd") == 0) {
            *out = 1;
            return true;
        }
        if (strcmp(item->valuestring, "even") == 0) {
            *out = 2;
            return true;
        }
    }
    return false;
}

/* ------------------------------------------------------------------ */
/* Reboot scheduling                                                  */
/* ------------------------------------------------------------------ */

static void reboot_timer_cb(void *arg)
{
    (void)arg;
    ESP_LOGW(HTTP_SERVER_TAG, "restarting to apply the new configuration");
    esp_restart();
}

static void schedule_reboot(uint32_t delay_ms)
{
    static esp_timer_handle_t reboot_timer;
    if (reboot_timer == NULL) {
        const esp_timer_create_args_t args = {
            .callback = reboot_timer_cb,
            .arg = NULL,
            .name = "cfg_reboot",
        };
        if (esp_timer_create(&args, &reboot_timer) != ESP_OK) {
            ESP_LOGE(HTTP_SERVER_TAG, "cannot create reboot timer");
            return;
        }
    }
    (void)esp_timer_start_once(reboot_timer, (uint64_t)delay_ms * 1000ULL);
    ESP_LOGI(HTTP_SERVER_TAG, "restart scheduled in %" PRIu32 " ms", delay_ms);
}

/* ------------------------------------------------------------------ */
/* HTTP handlers                                                      */
/* ------------------------------------------------------------------ */

static esp_err_t root_get_handler(httpd_req_t *req)
{
    httpd_resp_set_type(req, "text/html; charset=utf-8");
    httpd_resp_set_hdr(req, "Cache-Control", "no-store");
    return httpd_resp_send(req, (const char *)index_html_start,
                           HTTPD_RESP_USE_STRLEN);
}

static esp_err_t favicon_get_handler(httpd_req_t *req)
{
    httpd_resp_set_status(req, "204 No Content");
    return httpd_resp_send(req, "", 0);
}

static esp_err_t api_status_get_handler(httpd_req_t *req)
{
    cJSON *root = build_status_json();
    if (root == NULL) {
        return httpd_resp_send_500(req);
    }
    const esp_err_t err = send_json(req, root);
    cJSON_Delete(root);
    return err;
}

static esp_err_t api_config_get_handler(httpd_req_t *req)
{
    cJSON *root = build_config_json();
    if (root == NULL) {
        return httpd_resp_send_500(req);
    }
    const esp_err_t err = send_json(req, root);
    cJSON_Delete(root);
    return err;
}

static esp_err_t api_config_post_handler(httpd_req_t *req)
{
    char body[CONFIG_BODY_MAX];
    size_t body_len = 0;
    esp_err_t err = read_request_body(req, body, sizeof(body), &body_len);
    if (err == ESP_ERR_INVALID_SIZE) {
        return send_error_json(req, "413 Payload Too Large",
                               "configuration body too large");
    }
    if (err != ESP_OK) {
        return send_error_json(req, "400 Bad Request", "cannot read request body");
    }

    (void)body_len;
    cJSON *root = cJSON_Parse(body);
    if (root == NULL) {
        return send_error_json(req, "400 Bad Request", "invalid JSON");
    }

    app_config_t cfg;
    config_store_get(&cfg);

    uint32_t baud = cfg.baud;
    uint8_t data_bits = cfg.data_bits;
    uint8_t parity = cfg.parity;
    uint8_t stop_bits = cfg.stop_bits;
    uint8_t flow = cfg.flow;
    bool uart_changed = false;

    if (cJSON_GetObjectItemCaseSensitive(root, "baud") != NULL) {
        if (!json_get_u32(root, "baud", &baud)) {
            cJSON_Delete(root);
            return send_error_json(req, "400 Bad Request", "invalid baud");
        }
        uart_changed = true;
    }
    uint32_t num = 0;
    if (cJSON_GetObjectItemCaseSensitive(root, "data_bits") != NULL) {
        if (!json_get_u32(root, "data_bits", &num) || num > 8u) {
            cJSON_Delete(root);
            return send_error_json(req, "400 Bad Request", "invalid data_bits");
        }
        data_bits = (uint8_t)num;
        uart_changed = true;
    }
    if (cJSON_GetObjectItemCaseSensitive(root, "stop_bits") != NULL) {
        if (!json_get_u32(root, "stop_bits", &num) || num > 2u) {
            cJSON_Delete(root);
            return send_error_json(req, "400 Bad Request", "invalid stop_bits");
        }
        stop_bits = (uint8_t)num;
        uart_changed = true;
    }
    if (cJSON_GetObjectItemCaseSensitive(root, "flow") != NULL) {
        if (!json_get_u32(root, "flow", &num) || num > 1u) {
            cJSON_Delete(root);
            return send_error_json(req, "400 Bad Request", "invalid flow");
        }
        flow = (uint8_t)num;
        uart_changed = true;
    }
    if (cJSON_GetObjectItemCaseSensitive(root, "parity") != NULL) {
        if (!json_parse_parity(root, &parity)) {
            cJSON_Delete(root);
            return send_error_json(req, "400 Bad Request", "invalid parity");
        }
        uart_changed = true;
    }

    const char *name = NULL;
    bool name_changed = false;

    if (cJSON_GetObjectItemCaseSensitive(root, "name") != NULL) {
        if (!json_get_string(root, "name", &name) || name[0] == '\0' ||
            strlen(name) >= CONFIG_STORE_NAME_MAX) {
            cJSON_Delete(root);
            return send_error_json(req, "400 Bad Request", "invalid name");
        }
        name_changed = true;
    }

    uint16_t wsp_port = cfg.wsp_port;
    uint16_t raw_port = cfg.raw_port;
    bool ports_changed = false;
    if (cJSON_GetObjectItemCaseSensitive(root, "wsp_port") != NULL ||
        cJSON_GetObjectItemCaseSensitive(root, "raw_port") != NULL) {
        if (cJSON_GetObjectItemCaseSensitive(root, "wsp_port") != NULL) {
            if (!json_get_u32(root, "wsp_port", &num) || num == 0u || num > 65535u) {
                cJSON_Delete(root);
                return send_error_json(req, "400 Bad Request", "invalid wsp_port");
            }
            wsp_port = (uint16_t)num;
        }
        if (cJSON_GetObjectItemCaseSensitive(root, "raw_port") != NULL) {
            if (!json_get_u32(root, "raw_port", &num) || num == 0u || num > 65535u) {
                cJSON_Delete(root);
                return send_error_json(req, "400 Bad Request", "invalid raw_port");
            }
            raw_port = (uint16_t)num;
        }
        if (wsp_port == raw_port) {
            cJSON_Delete(root);
            return send_error_json(req, "400 Bad Request", "ports must differ");
        }
        ports_changed = true;
    }

    const char *wifi_ssid = NULL;
    const char *wifi_pass = NULL;
    bool wifi_changed = false;
    const cJSON *wifi = cJSON_GetObjectItemCaseSensitive(root, "wifi");
    if (cJSON_IsObject(wifi)) {
        if (json_get_string(wifi, "ssid", &wifi_ssid)) {
            wifi_changed = true;
        }
        if (json_get_string(wifi, "pass", &wifi_pass)) {
            wifi_changed = true;
        }
        if (wifi_changed && wifi_ssid == NULL) {
            if (!json_get_string(wifi, "ssid", &wifi_ssid)) {
                cJSON_Delete(root);
                return send_error_json(req, "400 Bad Request", "wifi.ssid required");
            }
        }
        if (wifi_changed && wifi_pass == NULL) {
            wifi_pass = "";
        }
    }

    /* Validate the complete UART format before touching the hardware. */
    if (uart_changed) {
        const uart_cfg_t want = {
            .baud = baud,
            .data_bits = data_bits,
            .parity = parity,
            .stop_bits = stop_bits,
            .flow = flow,
        };
        if (!uart_bridge_cfg_valid(&want)) {
            cJSON_Delete(root);
            return send_error_json(req, "400 Bad Request", "invalid UART format");
        }
        uart_cfg_t applied;
        memset(&applied, 0, sizeof(applied));
        err = uart_bridge_configure(&want, &applied);
        if (err != ESP_OK) {
            cJSON_Delete(root);
            return send_error_json(req, "409 Conflict", "cannot apply UART format");
        }
        err = config_store_set_uart(applied.baud, applied.data_bits,
                                    applied.parity, applied.stop_bits,
                                    applied.flow);
        if (err != ESP_OK) {
            cJSON_Delete(root);
            return send_error_json(req, "500 Internal Server Error",
                                   "cannot persist UART format");
        }
        (void)mdns_srv_refresh();
    }

    if (name_changed) {
        err = config_store_set_name(name);
        if (err != ESP_OK) {
            cJSON_Delete(root);
            return send_error_json(req, "500 Internal Server Error",
                                   "cannot persist name");
        }
        (void)mdns_srv_refresh();
    }

    if (ports_changed) {
        err = config_store_set_ports(wsp_port, raw_port);
        if (err != ESP_OK) {
            cJSON_Delete(root);
            return send_error_json(req, "500 Internal Server Error",
                                   "cannot persist ports");
        }
    }

    if (wifi_changed) {
        err = config_store_set_wifi(wifi_ssid, wifi_pass);
        if (err != ESP_OK) {
            cJSON_Delete(root);
            return send_error_json(req, "400 Bad Request",
                                   "invalid WiFi credentials");
        }
    }

    const bool reboot_needed = wifi_changed || ports_changed;

    cJSON *reply = build_config_json();
    esp_err_t send_err = ESP_OK;
    if (reply == NULL) {
        send_err = httpd_resp_send_500(req);
    } else {
        cJSON_AddBoolToObject(reply, "ok", true);
        cJSON_AddBoolToObject(reply, "reboot", reboot_needed);
        send_err = send_json(req, reply);
        cJSON_Delete(reply);
    }
    cJSON_Delete(root);

    if (reboot_needed) {
        schedule_reboot(1200);
    }
    return send_err;
}

static esp_err_t api_scan_get_handler(httpd_req_t *req)
{
    wifi_ap_record_t records[MAX_SCAN_RESULTS];
    uint16_t count = 0;
    const esp_err_t err = wifi_mgr_scan(records, MAX_SCAN_RESULTS, &count);
    if (err != ESP_OK) {
        return send_error_json(req, "500 Internal Server Error", "WiFi scan failed");
    }

    /* Strongest RSSI first. */
    for (uint16_t i = 1; i < count; i++) {
        const wifi_ap_record_t tmp = records[i];
        int j = (int)i - 1;
        while (j >= 0 && records[j].rssi < tmp.rssi) {
            records[j + 1] = records[j];
            j--;
        }
        records[j + 1] = tmp;
    }

    cJSON *array = cJSON_CreateArray();
    if (array == NULL) {
        return httpd_resp_send_500(req);
    }
    for (uint16_t i = 0; i < count; i++) {
        cJSON *item = cJSON_CreateObject();
        if (item == NULL) {
            continue;
        }
        cJSON_AddStringToObject(item, "ssid", (const char *)records[i].ssid);
        cJSON_AddNumberToObject(item, "rssi", records[i].rssi);
        cJSON_AddStringToObject(item, "auth", auth_name(records[i].authmode));
        cJSON_AddItemToArray(array, item);
    }
    const esp_err_t send_err = send_json(req, array);
    cJSON_Delete(array);
    return send_err;
}

static esp_err_t api_reboot_post_handler(httpd_req_t *req)
{
    cJSON *root = cJSON_CreateObject();
    if (root == NULL) {
        return httpd_resp_send_500(req);
    }
    cJSON_AddBoolToObject(root, "ok", true);
    cJSON_AddStringToObject(root, "message", "rebooting");
    const esp_err_t err = send_json(req, root);
    cJSON_Delete(root);
    schedule_reboot(500);
    return err;
}

/* ------------------------------------------------------------------ */
/* WebSocket terminal                                                 */
/* ------------------------------------------------------------------ */

/* TCP keepalive on the WS socket, so a browser that disappears without a
 * FIN/CLOSE does not hold the UART lock forever. */
static void ws_enable_tcp_keepalive(int fd)
{
    int opt = 1;
    (void)setsockopt(fd, SOL_SOCKET, SO_KEEPALIVE, &opt, sizeof(opt));
#if defined(TCP_KEEPIDLE) && defined(TCP_KEEPINTVL) && defined(TCP_KEEPCNT)
    const int idle = 60;
    const int intvl = 10;
    const int cnt = 3;
    (void)setsockopt(fd, IPPROTO_TCP, TCP_KEEPIDLE, &idle, sizeof(idle));
    (void)setsockopt(fd, IPPROTO_TCP, TCP_KEEPINTVL, &intvl, sizeof(intvl));
    (void)setsockopt(fd, IPPROTO_TCP, TCP_KEEPCNT, &cnt, sizeof(cnt));
#endif
}

static void ws_pump_task(void *arg)
{
    (void)arg;
    const httpd_handle_t hd = s_ws.hd;
    const int fd = s_ws.fd;
    uint8_t buf[512];

    for (;;) {
        if (httpd_ws_get_fd_info(hd, fd) != HTTPD_WS_CLIENT_WEBSOCKET) {
            break;
        }
        size_t got = 0;
        const esp_err_t err = uart_bridge_read(buf, sizeof(buf), 100, &got);
        if (err != ESP_OK || got == 0) {
            continue;
        }

        httpd_ws_frame_t frame;
        memset(&frame, 0, sizeof(frame));
        frame.type = HTTPD_WS_TYPE_BINARY;
        frame.payload = buf;
        frame.len = got;

        (void)xSemaphoreTake(s_ws_tx_lock, portMAX_DELAY);
        const esp_err_t send_err = httpd_ws_send_frame_async(hd, fd, &frame);
        (void)xSemaphoreGive(s_ws_tx_lock);
        if (send_err != ESP_OK) {
            break;
        }
    }

    tcp_server_release_uart(TCP_CLIENT_WS);
    ESP_LOGI(HTTP_SERVER_TAG, "WebSocket terminal detached from UART");
    vTaskDelete(NULL);
}

static esp_err_t ws_send_text(httpd_req_t *req, cJSON *root)
{
    if (root == NULL) {
        return ESP_FAIL;
    }
    char *text = cJSON_PrintUnformatted(root);
    if (text == NULL) {
        return ESP_FAIL;
    }
    httpd_ws_frame_t frame;
    memset(&frame, 0, sizeof(frame));
    frame.type = HTTPD_WS_TYPE_TEXT;
    frame.payload = (uint8_t *)text;
    frame.len = strlen(text);

    (void)xSemaphoreTake(s_ws_tx_lock, portMAX_DELAY);
    const esp_err_t err = httpd_ws_send_frame(req, &frame);
    (void)xSemaphoreGive(s_ws_tx_lock);
    cJSON_free(text);
    return err;
}

static esp_err_t ws_reply_ok(httpd_req_t *req, const char *cmd, cJSON *extra)
{
    cJSON *root = cJSON_CreateObject();
    if (root == NULL) {
        return ESP_FAIL;
    }
    cJSON_AddBoolToObject(root, "ok", true);
    if (cmd != NULL) {
        cJSON_AddStringToObject(root, "cmd", cmd);
    }
    if (extra != NULL) {
        cJSON *item = NULL;
        cJSON_ArrayForEach(item, extra)
        {
            cJSON_AddItemToObject(root, item->string, cJSON_Duplicate(item, true));
        }
        cJSON_Delete(extra);
    }
    const esp_err_t err = ws_send_text(req, root);
    cJSON_Delete(root);
    return err;
}

static esp_err_t ws_reply_error(httpd_req_t *req, const char *message)
{
    cJSON *root = cJSON_CreateObject();
    if (root == NULL) {
        return ESP_FAIL;
    }
    cJSON_AddBoolToObject(root, "ok", false);
    cJSON_AddStringToObject(root, "error", message);
    const esp_err_t err = ws_send_text(req, root);
    cJSON_Delete(root);
    return err;
}

static esp_err_t ws_handle_uart_cmd(httpd_req_t *req, const cJSON *cmd)
{
    uart_cfg_t cfg;
    uart_bridge_get_cfg(&cfg);

    uint32_t num = 0;
    if (json_get_u32(cmd, "baud", &num)) {
        cfg.baud = num;
    }
    if (json_get_u32(cmd, "data_bits", &num)) {
        if (num > 8u) {
            return ws_reply_error(req, "invalid data_bits");
        }
        cfg.data_bits = (uint8_t)num;
    }
    if (json_get_u32(cmd, "stop_bits", &num)) {
        if (num > 2u) {
            return ws_reply_error(req, "invalid stop_bits");
        }
        cfg.stop_bits = (uint8_t)num;
    }
    if (json_get_u32(cmd, "flow", &num)) {
        if (num > 1u) {
            return ws_reply_error(req, "invalid flow");
        }
        cfg.flow = (uint8_t)num;
    }
    if (cJSON_GetObjectItemCaseSensitive(cmd, "parity") != NULL) {
        uint8_t parity = 0;
        if (!json_parse_parity(cmd, &parity)) {
            return ws_reply_error(req, "invalid parity");
        }
        cfg.parity = parity;
    }

    uart_cfg_t applied;
    memset(&applied, 0, sizeof(applied));
    if (uart_bridge_configure(&cfg, &applied) != ESP_OK) {
        return ws_reply_error(req, "invalid UART configuration");
    }
    if (config_store_set_uart(applied.baud, applied.data_bits, applied.parity,
                              applied.stop_bits, applied.flow) != ESP_OK) {
        ESP_LOGW(HTTP_SERVER_TAG, "cannot persist UART format from WS");
    }
    (void)mdns_srv_refresh();

    cJSON *extra = cJSON_CreateObject();
    if (extra != NULL) {
        cJSON_AddNumberToObject(extra, "baud", (double)applied.baud);
        cJSON_AddNumberToObject(extra, "data_bits", applied.data_bits);
        cJSON_AddStringToObject(extra, "parity", parity_name(applied.parity));
        cJSON_AddNumberToObject(extra, "stop_bits", applied.stop_bits);
        cJSON_AddNumberToObject(extra, "flow", applied.flow);
    }
    return ws_reply_ok(req, "uart", extra);
}

static esp_err_t ws_handle_line_cmd(httpd_req_t *req, const cJSON *cmd)
{
    const uint8_t current = uart_bridge_line_status();
    bool dtr = (current & WSP_ST_DTR) != 0;
    bool rts = (current & WSP_ST_RTS) != 0;
    bool brk = (current & WSP_ST_BREAK) != 0;

    (void)json_get_bool(cmd, "dtr", &dtr);
    (void)json_get_bool(cmd, "rts", &rts);
    (void)json_get_bool(cmd, "break", &brk);

    uint8_t state = 0;
    if (uart_bridge_line_ctrl(dtr, rts, brk, &state) != ESP_OK) {
        return ws_reply_error(req, "line control failed");
    }

    cJSON *extra = cJSON_CreateObject();
    if (extra != NULL) {
        cJSON_AddNumberToObject(extra, "state", state);
        cJSON_AddBoolToObject(extra, "dtr", dtr);
        cJSON_AddBoolToObject(extra, "rts", rts);
        cJSON_AddBoolToObject(extra, "break", brk);
    }
    return ws_reply_ok(req, "line", extra);
}

static esp_err_t ws_data_handler(httpd_req_t *req)
{
    /* The pump task may have released the UART (client vanished or a send
     * error) while the session was still open; do not touch the UART then. */
    if (tcp_server_client_kind() != TCP_CLIENT_WS) {
        return ESP_FAIL;
    }

    httpd_ws_frame_t frame;
    memset(&frame, 0, sizeof(frame));
    frame.type = HTTPD_WS_TYPE_TEXT;

    esp_err_t err = httpd_ws_recv_frame(req, &frame, 0);
    if (err != ESP_OK) {
        return err;
    }
    if (frame.len == 0) {
        return ESP_OK;
    }
    if (frame.len > WS_MAX_TEXT_FRAME) {
        ESP_LOGW(HTTP_SERVER_TAG, "WebSocket frame too long (%u)", (unsigned)frame.len);
        return ESP_FAIL;
    }

    uint8_t payload[WS_MAX_TEXT_FRAME + 1];
    frame.payload = payload;
    err = httpd_ws_recv_frame(req, &frame, frame.len);
    if (err != ESP_OK) {
        return err;
    }
    payload[frame.len] = '\0';

    if (frame.type != HTTPD_WS_TYPE_TEXT) {
        return ESP_OK;
    }

    cJSON *root = cJSON_Parse((const char *)payload);
    if (root == NULL) {
        return ws_reply_error(req, "invalid JSON");
    }

    const cJSON *cmd = cJSON_GetObjectItemCaseSensitive(root, "cmd");
    if (!cJSON_IsString(cmd)) {
        cJSON_Delete(root);
        return ws_reply_error(req, "missing cmd");
    }

    if (strcmp(cmd->valuestring, "uart") == 0) {
        err = ws_handle_uart_cmd(req, root);
    } else if (strcmp(cmd->valuestring, "line") == 0) {
        err = ws_handle_line_cmd(req, root);
    } else if (strcmp(cmd->valuestring, "ping") == 0) {
        cJSON *extra = cJSON_CreateObject();
        if (extra != NULL) {
            cJSON_AddNumberToObject(extra, "uptime",
                                    (double)(esp_timer_get_time() / 1000000LL));
        }
        err = ws_reply_ok(req, "pong", extra);
    } else if (strcmp(cmd->valuestring, "stats") == 0) {
        wsp_stats_t stats;
        tcp_server_get_stats(&stats);
        cJSON *extra = cJSON_CreateObject();
        if (extra != NULL) {
            cJSON_AddNumberToObject(extra, "uart_rx_bytes",
                                    (double)stats.uart_rx_bytes);
            cJSON_AddNumberToObject(extra, "uart_tx_bytes",
                                    (double)stats.uart_tx_bytes);
            cJSON_AddNumberToObject(extra, "uart_overflows",
                                    (double)stats.uart_overflows);
        }
        err = ws_reply_ok(req, "stats", extra);
    } else if (strcmp(cmd->valuestring, "status") == 0) {
        cJSON *status = build_status_json();
        err = ws_send_text(req, status);
        cJSON_Delete(status);
    } else {
        err = ws_reply_error(req, "unknown cmd");
    }

    cJSON_Delete(root);
    return err;
}

static esp_err_t ws_handler(httpd_req_t *req)
{
    /* A fresh HTTP GET means the WebSocket handshake; any other method value
     * is an already established WS data frame (the HTTP server dispatches one
     * handler call per frame). */
    if (req->method == HTTP_GET) {
        if (!tcp_server_acquire_uart(TCP_CLIENT_WS, 0)) {
            ESP_LOGW(HTTP_SERVER_TAG, "UART busy, rejecting WebSocket terminal");
            cJSON *root = cJSON_CreateObject();
            if (root != NULL) {
                cJSON_AddBoolToObject(root, "ok", false);
                cJSON_AddStringToObject(root, "error", "serial port busy");
                (void)ws_send_text(req, root);
                cJSON_Delete(root);
            }
            return ESP_FAIL;
        }

        uart_bridge_flush_input();
        (void)uart_bridge_line_ctrl(false, false, false, NULL);

        s_ws.hd = req->handle;
        s_ws.fd = httpd_req_to_sockfd(req);
        s_ws.task = NULL;
        ws_enable_tcp_keepalive(s_ws.fd);
        const BaseType_t ok = xTaskCreate(ws_pump_task, "ws_pump",
                                          WS_PUMP_TASK_STACK, NULL,
                                          WS_PUMP_TASK_PRIO, &s_ws.task);
        if (ok != pdPASS) {
            ESP_LOGE(HTTP_SERVER_TAG, "cannot create WebSocket pump task");
            tcp_server_release_uart(TCP_CLIENT_WS);
            return ESP_FAIL;
        }

        ESP_LOGI(HTTP_SERVER_TAG, "WebSocket terminal attached to UART (fd %d)",
                 s_ws.fd);
        cJSON *status = build_status_json();
        if (status != NULL) {
            (void)ws_send_text(req, status);
            cJSON_Delete(status);
        }
        return ESP_OK;
    }

    return ws_data_handler(req);
}

/* ------------------------------------------------------------------ */
/* Registration                                                       */
/* ------------------------------------------------------------------ */

static const httpd_uri_t s_uri_root = {
    .uri = "/",
    .method = HTTP_GET,
    .handler = root_get_handler,
    .user_ctx = NULL,
};

static const httpd_uri_t s_uri_favicon = {
    .uri = "/favicon.ico",
    .method = HTTP_GET,
    .handler = favicon_get_handler,
    .user_ctx = NULL,
};

static const httpd_uri_t s_uri_status = {
    .uri = "/api/status",
    .method = HTTP_GET,
    .handler = api_status_get_handler,
    .user_ctx = NULL,
};

static const httpd_uri_t s_uri_config_get = {
    .uri = "/api/config",
    .method = HTTP_GET,
    .handler = api_config_get_handler,
    .user_ctx = NULL,
};

static const httpd_uri_t s_uri_config_post = {
    .uri = "/api/config",
    .method = HTTP_POST,
    .handler = api_config_post_handler,
    .user_ctx = NULL,
};

static const httpd_uri_t s_uri_scan = {
    .uri = "/api/scan",
    .method = HTTP_GET,
    .handler = api_scan_get_handler,
    .user_ctx = NULL,
};

static const httpd_uri_t s_uri_reboot = {
    .uri = "/api/reboot",
    .method = HTTP_POST,
    .handler = api_reboot_post_handler,
    .user_ctx = NULL,
};

static const httpd_uri_t s_uri_ws = {
    .uri = "/ws",
    .method = HTTP_GET,
    .handler = ws_handler,
    .user_ctx = NULL,
    .is_websocket = true,
};

esp_err_t http_server_start(void)
{
    if (s_ws_tx_lock == NULL) {
        s_ws_tx_lock = xSemaphoreCreateMutex();
        if (s_ws_tx_lock == NULL) {
            ESP_LOGE(HTTP_SERVER_TAG, "cannot create WebSocket tx mutex");
            return ESP_ERR_NO_MEM;
        }
    }

    httpd_config_t config = HTTPD_DEFAULT_CONFIG();
    config.server_port = CONFIG_WSP_HTTP_PORT;
    config.max_uri_handlers = 8;
    config.max_open_sockets = 4;
    config.stack_size = 10240;
    config.lru_purge_enable = true;
    config.keep_alive_enable = true;

    esp_err_t err = httpd_start(&s_server, &config);
    if (err != ESP_OK) {
        ESP_LOGE(HTTP_SERVER_TAG, "httpd_start failed: %s", esp_err_to_name(err));
        return err;
    }

    const httpd_uri_t *handlers[] = {
        &s_uri_root, &s_uri_favicon, &s_uri_status, &s_uri_config_get,
        &s_uri_config_post, &s_uri_scan, &s_uri_reboot, &s_uri_ws,
    };
    for (size_t i = 0; i < sizeof(handlers) / sizeof(handlers[0]); i++) {
        err = httpd_register_uri_handler(s_server, handlers[i]);
        if (err != ESP_OK) {
            ESP_LOGE(HTTP_SERVER_TAG, "cannot register uri %s: %s",
                     handlers[i]->uri, esp_err_to_name(err));
            return err;
        }
    }

    ESP_LOGI(HTTP_SERVER_TAG, "HTTP server on port %d (UI + JSON API + /ws)",
             config.server_port);
    return ESP_OK;
}
