/*
 * app_main.c — boot sequence for the wireless serial adapter.
 *
 *   NVS -> configuration -> UART1 bridge -> status LED -> WiFi (STA or SoftAP)
 *   -> mDNS -> WSP/RAW TCP servers -> HTTP/UI server -> supervision task.
 */

#include <inttypes.h>
#include <string.h>

#include "esp_err.h"
#include "esp_log.h"
#include "esp_system.h"
#include "esp_task_wdt.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "nvs_flash.h"
#include "sdkconfig.h"

#include "config_store.h"
#include "http_server.h"
#include "mdns_srv.h"
#include "status_led.h"
#include "tcp_server.h"
#include "uart_bridge.h"
#include "wifi_mgr.h"

#define APP_MAIN_TAG "app_main"

#define HEALTH_TASK_STACK 3072
#define HEALTH_TASK_PRIO  1
#define HEALTH_PERIOD_MS  2000
#define HEALTH_LOG_EVERY  15 /* ~30 s */

static const char *app_mode_name(wifi_mgr_mode_t mode)
{
    switch (mode) {
    case WIFI_MGR_MODE_STA: return "sta";
    case WIFI_MGR_MODE_AP:  return "softap";
    default:                return "none";
    }
}

static const char *app_client_name(void)
{
    switch (tcp_server_client_kind()) {
    case TCP_CLIENT_WSP: return "wsp";
    case TCP_CLIENT_RAW: return "raw";
    case TCP_CLIENT_WS:  return "ws";
    default:             return "none";
    }
}

/* Feeds the task watchdog and periodically reports health counters.  All
 * worker tasks are event driven (blocking socket / stream buffer waits), so the
 * idle task keeps running and the system stays responsive. */
static void health_task(void *arg)
{
    (void)arg;
#ifdef CONFIG_ESP_TASK_WDT_INIT
    (void)esp_task_wdt_add(NULL);
#endif
    uint32_t ticks = 0;

    for (;;) {
#ifdef CONFIG_ESP_TASK_WDT_INIT
        (void)esp_task_wdt_reset();
#endif
        if (++ticks >= HEALTH_LOG_EVERY) {
            ticks = 0;
            wifi_mgr_status_t wifi;
            wifi_mgr_get_status(&wifi);
            wsp_stats_t stats;
            tcp_server_get_stats(&stats);
            ESP_LOGI(APP_MAIN_TAG,
                     "heap=%" PRIu32 "B mode=%s ip=%s client=%s "
                     "uart_rx=%" PRIu32 " uart_tx=%" PRIu32 " overflow=%" PRIu32
                     " crc_err=%" PRIu32,
                     esp_get_free_heap_size(), app_mode_name(wifi.mode), wifi.ip,
                     app_client_name(), stats.uart_rx_bytes,
                     stats.uart_tx_bytes, stats.uart_overflows, stats.crc_errors);
        }
        vTaskDelay(pdMS_TO_TICKS(HEALTH_PERIOD_MS));
    }
}

void app_main(void)
{
    ESP_LOGI(APP_MAIN_TAG, "Wireless Serial v%d.%d.%d / ESP32-C3 (WSP/1)",
             CONFIG_WSP_FW_VERSION_MAJOR, CONFIG_WSP_FW_VERSION_MINOR,
             CONFIG_WSP_FW_VERSION_PATCH);

    esp_err_t err = nvs_flash_init();
    if (err == ESP_ERR_NVS_NO_FREE_PAGES || err == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        ESP_LOGW(APP_MAIN_TAG, "NVS needs to be erased (%s)", esp_err_to_name(err));
        ESP_ERROR_CHECK(nvs_flash_erase());
        err = nvs_flash_init();
    }
    ESP_ERROR_CHECK(err);

    ESP_ERROR_CHECK(config_store_init());

    app_config_t cfg;
    config_store_get(&cfg);

    const uart_cfg_t uart = {
        .baud = cfg.baud,
        .data_bits = cfg.data_bits,
        .parity = cfg.parity,
        .stop_bits = cfg.stop_bits,
        .flow = cfg.flow,
    };
    ESP_ERROR_CHECK(uart_bridge_init(&uart));

    uart_cfg_t active;
    uart_bridge_get_cfg(&active);
    ESP_LOGI(APP_MAIN_TAG, "device \"%s\", UART1 %" PRIu32 " %u%c%u flow=%u",
             cfg.name, active.baud, active.data_bits,
             (active.parity == 1u) ? 'O' : ((active.parity == 2u) ? 'E' : 'N'),
             active.stop_bits, active.flow);

    (void)status_led_init();

    err = wifi_mgr_init();
    if (err != ESP_OK) {
        ESP_LOGE(APP_MAIN_TAG, "WiFi initialisation failed: %s",
                 esp_err_to_name(err));
    }

    wifi_mgr_status_t wifi;
    wifi_mgr_get_status(&wifi);
    ESP_LOGI(APP_MAIN_TAG, "network: mode=%s ssid=\"%s\" ip=%s rssi=%d",
             app_mode_name(wifi.mode), wifi.ssid, wifi.ip, wifi.rssi);

    err = mdns_srv_start();
    if (err != ESP_OK) {
        ESP_LOGW(APP_MAIN_TAG, "mDNS unavailable: %s", esp_err_to_name(err));
    }

    err = tcp_server_start();
    if (err != ESP_OK) {
        ESP_LOGE(APP_MAIN_TAG, "TCP servers failed to start: %s",
                 esp_err_to_name(err));
    }

    err = http_server_start();
    if (err != ESP_OK) {
        ESP_LOGE(APP_MAIN_TAG, "HTTP server failed to start: %s",
                 esp_err_to_name(err));
    }

    config_store_get(&cfg);
    ESP_LOGI(APP_MAIN_TAG,
             "ready: name=\"%s\" WSP=%u RAW=%u HTTP=%u mDNS=_wserial._tcp",
             cfg.name, cfg.wsp_port, cfg.raw_port, CONFIG_WSP_HTTP_PORT);

    const BaseType_t ok = xTaskCreate(health_task, "health", HEALTH_TASK_STACK,
                                      NULL, HEALTH_TASK_PRIO, NULL);
    if (ok != pdPASS) {
        ESP_LOGE(APP_MAIN_TAG, "cannot create health task");
    }
}
