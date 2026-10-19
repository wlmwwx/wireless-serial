/*
 * config_store.c — NVS backed configuration with Kconfig defaults.
 *
 * NVS namespace: "wserial".  Keys are short (<= 15 chars) and all writes are
 * committed immediately so a power loss never leaves a partially applied
 * configuration in RAM only.
 */

#include "config_store.h"

#include <inttypes.h>
#include <string.h>

#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "nvs.h"
#include "nvs_flash.h"
#include "sdkconfig.h"

#include "uart_bridge.h"

#define CONFIG_STORE_TAG      "config_store"
#define CONFIG_STORE_NVS_NS   "wserial"

static const char *const KEY_NAME  = "name";
static const char *const KEY_BAUD  = "baud";
static const char *const KEY_DBITS = "dbits";
static const char *const KEY_PAR   = "par";
static const char *const KEY_STOP  = "stop";
static const char *const KEY_FLOW  = "flow";
static const char *const KEY_WPORT = "wport";
static const char *const KEY_RPORT = "rport";
static const char *const KEY_SSID  = "ssid";
static const char *const KEY_PASS  = "pass";

static app_config_t s_cfg;
static nvs_handle_t s_nvs;
static SemaphoreHandle_t s_lock;
static bool s_ready;

/* ------------------------------------------------------------------ */
/* Defaults and NVS helpers                                           */
/* ------------------------------------------------------------------ */

static void load_defaults(app_config_t *cfg)
{
    memset(cfg, 0, sizeof(*cfg));
    (void)strlcpy(cfg->name, CONFIG_WSP_DEVICE_NAME, sizeof(cfg->name));
    cfg->baud = (uint32_t)CONFIG_WSP_DEFAULT_BAUD;
    cfg->data_bits = (uint8_t)CONFIG_WSP_DEFAULT_DATA_BITS;
    cfg->parity = (uint8_t)CONFIG_WSP_DEFAULT_PARITY;
    cfg->stop_bits = (uint8_t)CONFIG_WSP_DEFAULT_STOP_BITS;
    cfg->flow = 0;
    cfg->wsp_port = (uint16_t)CONFIG_WSP_WSP_PORT;
    cfg->raw_port = (uint16_t)CONFIG_WSP_RAW_PORT;
    (void)strlcpy(cfg->wifi_ssid, CONFIG_WSP_WIFI_SSID, sizeof(cfg->wifi_ssid));
    (void)strlcpy(cfg->wifi_pass, CONFIG_WSP_WIFI_PASSWORD, sizeof(cfg->wifi_pass));
    cfg->wifi_configured = (cfg->wifi_ssid[0] != '\0');
}

static uint32_t read_u32(const char *key, uint32_t fallback)
{
    uint32_t value = 0;
    if (nvs_get_u32(s_nvs, key, &value) == ESP_OK) {
        return value;
    }
    return fallback;
}

static uint8_t read_u8(const char *key, uint8_t fallback)
{
    uint8_t value = 0;
    if (nvs_get_u8(s_nvs, key, &value) == ESP_OK) {
        return value;
    }
    return fallback;
}

static void read_str(const char *key, char *out, size_t cap)
{
    if (out == NULL || cap == 0) {
        return;
    }
    size_t len = cap;
    if (nvs_get_str(s_nvs, key, out, &len) != ESP_OK) {
        /* keep the default already stored in *out */
    }
}

static esp_err_t write_u32(const char *key, uint32_t value)
{
    const esp_err_t err = nvs_set_u32(s_nvs, key, value);
    if (err != ESP_OK) {
        ESP_LOGE(CONFIG_STORE_TAG, "nvs_set_u32(%s) failed: %s", key, esp_err_to_name(err));
    }
    return err;
}

static esp_err_t write_u8(const char *key, uint8_t value)
{
    const esp_err_t err = nvs_set_u8(s_nvs, key, value);
    if (err != ESP_OK) {
        ESP_LOGE(CONFIG_STORE_TAG, "nvs_set_u8(%s) failed: %s", key, esp_err_to_name(err));
    }
    return err;
}

static esp_err_t write_str(const char *key, const char *value)
{
    const esp_err_t err = nvs_set_str(s_nvs, key, value);
    if (err != ESP_OK) {
        ESP_LOGE(CONFIG_STORE_TAG, "nvs_set_str(%s) failed: %s", key, esp_err_to_name(err));
    }
    return err;
}

/* Persist the UART format part of the snapshot.  Caller holds s_lock. */
static esp_err_t persist_uart(void)
{
    esp_err_t err = write_u32(KEY_BAUD, s_cfg.baud);
    if (err == ESP_OK) {
        err = write_u8(KEY_DBITS, s_cfg.data_bits);
    }
    if (err == ESP_OK) {
        err = write_u8(KEY_PAR, s_cfg.parity);
    }
    if (err == ESP_OK) {
        err = write_u8(KEY_STOP, s_cfg.stop_bits);
    }
    if (err == ESP_OK) {
        err = write_u8(KEY_FLOW, s_cfg.flow);
    }
    if (err == ESP_OK) {
        err = nvs_commit(s_nvs);
    }
    return err;
}

static bool uart_fields_valid(const app_config_t *cfg)
{
    const uart_cfg_t uart = {
        .baud = cfg->baud,
        .data_bits = cfg->data_bits,
        .parity = cfg->parity,
        .stop_bits = cfg->stop_bits,
        .flow = cfg->flow,
    };
    return uart_bridge_cfg_valid(&uart);
}

/* ------------------------------------------------------------------ */
/* Public API                                                         */
/* ------------------------------------------------------------------ */

esp_err_t config_store_init(void)
{
    if (s_ready) {
        return ESP_OK;
    }

    s_lock = xSemaphoreCreateMutex();
    if (s_lock == NULL) {
        ESP_LOGE(CONFIG_STORE_TAG, "cannot create mutex");
        return ESP_ERR_NO_MEM;
    }

    esp_err_t err = nvs_open(CONFIG_STORE_NVS_NS, NVS_READWRITE, &s_nvs);
    if (err != ESP_OK) {
        ESP_LOGE(CONFIG_STORE_TAG, "nvs_open failed: %s", esp_err_to_name(err));
        return err;
    }

    load_defaults(&s_cfg);

    read_str(KEY_NAME, s_cfg.name, sizeof(s_cfg.name));
    s_cfg.baud = read_u32(KEY_BAUD, s_cfg.baud);
    s_cfg.data_bits = read_u8(KEY_DBITS, s_cfg.data_bits);
    s_cfg.parity = read_u8(KEY_PAR, s_cfg.parity);
    s_cfg.stop_bits = read_u8(KEY_STOP, s_cfg.stop_bits);
    s_cfg.flow = read_u8(KEY_FLOW, s_cfg.flow);
    s_cfg.wsp_port = (uint16_t)read_u32(KEY_WPORT, s_cfg.wsp_port);
    s_cfg.raw_port = (uint16_t)read_u32(KEY_RPORT, s_cfg.raw_port);
    read_str(KEY_SSID, s_cfg.wifi_ssid, sizeof(s_cfg.wifi_ssid));
    read_str(KEY_PASS, s_cfg.wifi_pass, sizeof(s_cfg.wifi_pass));

    /* Sanitise anything that may have been written by an older firmware. */
    if (s_cfg.name[0] == '\0') {
        (void)strlcpy(s_cfg.name, CONFIG_WSP_DEVICE_NAME, sizeof(s_cfg.name));
    }
    if (!uart_fields_valid(&s_cfg)) {
        ESP_LOGW(CONFIG_STORE_TAG, "stored UART format invalid, using Kconfig defaults");
        s_cfg.baud = (uint32_t)CONFIG_WSP_DEFAULT_BAUD;
        s_cfg.data_bits = (uint8_t)CONFIG_WSP_DEFAULT_DATA_BITS;
        s_cfg.parity = (uint8_t)CONFIG_WSP_DEFAULT_PARITY;
        s_cfg.stop_bits = (uint8_t)CONFIG_WSP_DEFAULT_STOP_BITS;
        s_cfg.flow = 0;
    }
    if (s_cfg.wsp_port == 0u) {
        s_cfg.wsp_port = (uint16_t)CONFIG_WSP_WSP_PORT;
    }
    if (s_cfg.raw_port == 0u) {
        s_cfg.raw_port = (uint16_t)CONFIG_WSP_RAW_PORT;
    }
    s_cfg.wifi_configured = (s_cfg.wifi_ssid[0] != '\0');

    s_ready = true;
    ESP_LOGI(CONFIG_STORE_TAG, "name=\"%s\" uart=%" PRIu32 " %u%c%u flow=%u ports=%u/%u wifi=%s",
             s_cfg.name, s_cfg.baud, s_cfg.data_bits,
             (s_cfg.parity == 1u) ? 'O' : ((s_cfg.parity == 2u) ? 'E' : 'N'),
             s_cfg.stop_bits, s_cfg.flow, s_cfg.wsp_port, s_cfg.raw_port,
             s_cfg.wifi_configured ? "configured" : "not configured");
    return ESP_OK;
}

void config_store_get(app_config_t *out)
{
    if (out == NULL) {
        return;
    }
    if (!s_ready || s_lock == NULL) {
        load_defaults(out);
        return;
    }
    (void)xSemaphoreTake(s_lock, portMAX_DELAY);
    *out = s_cfg;
    (void)xSemaphoreGive(s_lock);
}

esp_err_t config_store_set_uart(uint32_t baud, uint8_t data_bits, uint8_t parity,
                                uint8_t stop_bits, uint8_t flow)
{
    const uart_cfg_t cfg = {
        .baud = baud,
        .data_bits = data_bits,
        .parity = parity,
        .stop_bits = stop_bits,
        .flow = flow,
    };
    if (!uart_bridge_cfg_valid(&cfg)) {
        return ESP_ERR_INVALID_ARG;
    }
    if (!s_ready) {
        return ESP_ERR_INVALID_STATE;
    }

    (void)xSemaphoreTake(s_lock, portMAX_DELAY);
    const app_config_t old = s_cfg;
    s_cfg.baud = baud;
    s_cfg.data_bits = data_bits;
    s_cfg.parity = parity;
    s_cfg.stop_bits = stop_bits;
    s_cfg.flow = flow;
    const esp_err_t err = persist_uart();
    if (err != ESP_OK) {
        s_cfg = old;
    }
    (void)xSemaphoreGive(s_lock);
    return err;
}

esp_err_t config_store_set_name(const char *name)
{
    if (name == NULL || name[0] == '\0' || strlen(name) >= sizeof(s_cfg.name)) {
        return ESP_ERR_INVALID_ARG;
    }
    if (!s_ready) {
        return ESP_ERR_INVALID_STATE;
    }

    (void)xSemaphoreTake(s_lock, portMAX_DELAY);
    const app_config_t old = s_cfg;
    (void)strlcpy(s_cfg.name, name, sizeof(s_cfg.name));
    esp_err_t err = write_str(KEY_NAME, s_cfg.name);
    if (err == ESP_OK) {
        err = nvs_commit(s_nvs);
    }
    if (err != ESP_OK) {
        s_cfg = old;
    }
    (void)xSemaphoreGive(s_lock);
    return err;
}

esp_err_t config_store_set_ports(uint16_t wsp_port, uint16_t raw_port)
{
    if (wsp_port == 0u || raw_port == 0u || wsp_port == raw_port) {
        return ESP_ERR_INVALID_ARG;
    }
    if (!s_ready) {
        return ESP_ERR_INVALID_STATE;
    }

    (void)xSemaphoreTake(s_lock, portMAX_DELAY);
    const app_config_t old = s_cfg;
    s_cfg.wsp_port = wsp_port;
    s_cfg.raw_port = raw_port;
    esp_err_t err = write_u32(KEY_WPORT, wsp_port);
    if (err == ESP_OK) {
        err = write_u32(KEY_RPORT, raw_port);
    }
    if (err == ESP_OK) {
        err = nvs_commit(s_nvs);
    }
    if (err != ESP_OK) {
        s_cfg = old;
    }
    (void)xSemaphoreGive(s_lock);
    return err;
}

esp_err_t config_store_set_wifi(const char *ssid, const char *pass)
{
    if (ssid == NULL || pass == NULL) {
        return ESP_ERR_INVALID_ARG;
    }
    const size_t ssid_len = strlen(ssid);
    const size_t pass_len = strlen(pass);
    if (ssid_len >= sizeof(s_cfg.wifi_ssid) || pass_len >= sizeof(s_cfg.wifi_pass)) {
        return ESP_ERR_INVALID_ARG;
    }
    if (pass_len > 0 && pass_len < 8) {
        return ESP_ERR_INVALID_ARG;
    }
    if (!s_ready) {
        return ESP_ERR_INVALID_STATE;
    }

    (void)xSemaphoreTake(s_lock, portMAX_DELAY);
    const app_config_t old = s_cfg;
    (void)strlcpy(s_cfg.wifi_ssid, ssid, sizeof(s_cfg.wifi_ssid));
    (void)strlcpy(s_cfg.wifi_pass, pass, sizeof(s_cfg.wifi_pass));
    s_cfg.wifi_configured = (s_cfg.wifi_ssid[0] != '\0');

    esp_err_t err = write_str(KEY_SSID, s_cfg.wifi_ssid);
    if (err == ESP_OK) {
        err = write_str(KEY_PASS, s_cfg.wifi_pass);
    }
    if (err == ESP_OK) {
        err = nvs_commit(s_nvs);
    }
    if (err != ESP_OK) {
        s_cfg = old;
    }
    (void)xSemaphoreGive(s_lock);
    return err;
}
