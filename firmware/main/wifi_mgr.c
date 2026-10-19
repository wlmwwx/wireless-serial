/*
 * wifi_mgr.c — station connection with SoftAP fallback.
 *
 * Boot sequence:
 *   1. If NVS/Kconfig holds WiFi credentials: STA mode, up to
 *      CONFIG_WSP_STA_MAX_RETRIES attempts of CONFIG_WSP_STA_CONNECT_TIMEOUT_MS.
 *   2. On success: stay in STA mode (runtime disconnects are retried forever in
 *      the background by the event handler).
 *   3. On failure or no credentials: SoftAP "wserial-XXXXXX" (APSTA mode, so
 *      the HTTP configuration page can also scan for networks).
 */

#include "wifi_mgr.h"

#include <stdio.h>
#include <string.h>

#include "esp_event.h"
#include "esp_log.h"
#include "esp_mac.h"
#include "esp_netif.h"
#include "esp_wifi_default.h"
#include "freertos/FreeRTOS.h"
#include "freertos/event_groups.h"
#include "freertos/semphr.h"
#include "freertos/task.h"
#include "sdkconfig.h"

#include "config_store.h"

#define WIFI_MGR_TAG "wifi_mgr"

#define WIFI_GOT_IP_BIT BIT0
#define WIFI_FAIL_BIT   BIT1

static EventGroupHandle_t s_events;
static SemaphoreHandle_t s_lock;
static wifi_mgr_status_t s_status;
static esp_netif_t *s_sta_netif;
static esp_netif_t *s_ap_netif;
static char s_ap_ssid[33];

/* During boot the disconnect handler only reports failure; once connected it
 * keeps reconnecting in the background.  While in SoftAP fallback we never
 * auto-connect the station. */
static bool s_boot_phase = true;

static void status_set_mode(wifi_mgr_mode_t mode)
{
    if (s_lock == NULL) {
        return;
    }
    (void)xSemaphoreTake(s_lock, portMAX_DELAY);
    s_status.mode = mode;
    (void)xSemaphoreGive(s_lock);
}

static void on_network_event(void *arg, esp_event_base_t base, int32_t id, void *data)
{
    (void)arg;

    if (base == WIFI_EVENT && id == WIFI_EVENT_STA_START) {
        ESP_LOGI(WIFI_MGR_TAG, "station interface started");
        return;
    }

    if (base == WIFI_EVENT && id == WIFI_EVENT_STA_DISCONNECTED) {
        const wifi_event_sta_disconnected_t *ev =
            (const wifi_event_sta_disconnected_t *)data;
        ESP_LOGW(WIFI_MGR_TAG, "station disconnected (reason %d)", ev->reason);

        if (s_lock != NULL) {
            (void)xSemaphoreTake(s_lock, portMAX_DELAY);
            s_status.connected = false;
            s_status.rssi = 0;
            (void)xSemaphoreGive(s_lock);
        }

        if (s_boot_phase) {
            (void)xEventGroupSetBits(s_events, WIFI_FAIL_BIT);
        } else {
            const esp_err_t err = esp_wifi_connect();
            if (err != ESP_OK) {
                ESP_LOGE(WIFI_MGR_TAG, "reconnect failed: %s", esp_err_to_name(err));
            }
        }
        return;
    }

    if (base == WIFI_EVENT && id == WIFI_EVENT_AP_START) {
        ESP_LOGI(WIFI_MGR_TAG, "SoftAP interface started");
        return;
    }

    if (base == IP_EVENT && id == IP_EVENT_STA_GOT_IP) {
        const ip_event_got_ip_t *ev = (const ip_event_got_ip_t *)data;
        char ip[16] = {0};
        (void)esp_ip4addr_ntoa(&ev->ip_info.ip, ip, sizeof(ip));

        wifi_ap_record_t ap;
        memset(&ap, 0, sizeof(ap));
        char ssid[sizeof(s_status.ssid)] = {0};
        int8_t rssi = 0;
        if (esp_wifi_sta_get_ap_info(&ap) == ESP_OK) {
            rssi = ap.rssi;
            (void)strlcpy(ssid, (const char *)ap.ssid, sizeof(ssid));
        }

        if (s_lock != NULL) {
            (void)xSemaphoreTake(s_lock, portMAX_DELAY);
            s_status.mode = WIFI_MGR_MODE_STA;
            s_status.connected = true;
            (void)strlcpy(s_status.ip, ip, sizeof(s_status.ip));
            (void)strlcpy(s_status.ssid, ssid, sizeof(s_status.ssid));
            s_status.rssi = rssi;
            (void)xSemaphoreGive(s_lock);
        }
        ESP_LOGI(WIFI_MGR_TAG, "connected to \"%s\" (rssi %d), IP %s", ssid, rssi, ip);
        (void)xEventGroupSetBits(s_events, WIFI_GOT_IP_BIT);
        return;
    }

    if (base == IP_EVENT && id == IP_EVENT_STA_LOST_IP) {
        if (s_lock != NULL) {
            (void)xSemaphoreTake(s_lock, portMAX_DELAY);
            s_status.connected = false;
            s_status.ip[0] = '\0';
            (void)xSemaphoreGive(s_lock);
        }
    }
}

static esp_err_t start_station(const app_config_t *cfg)
{
    wifi_config_t wc;
    memset(&wc, 0, sizeof(wc));
    (void)strlcpy((char *)wc.sta.ssid, cfg->wifi_ssid, sizeof(wc.sta.ssid));
    (void)strlcpy((char *)wc.sta.password, cfg->wifi_pass, sizeof(wc.sta.password));
    wc.sta.threshold.authmode = WIFI_AUTH_OPEN;
    wc.sta.pmf_cfg.capable = true;
    wc.sta.pmf_cfg.required = false;

    esp_err_t err = esp_wifi_set_mode(WIFI_MODE_STA);
    if (err != ESP_OK) {
        return err;
    }
    err = esp_wifi_set_config(WIFI_IF_STA, &wc);
    if (err != ESP_OK) {
        return err;
    }
    return esp_wifi_start();
}

static bool station_connect_with_retries(uint8_t *attempts_used)
{
    bool connected = false;
    for (uint8_t attempt = 1; attempt <= (uint8_t)CONFIG_WSP_STA_MAX_RETRIES; attempt++) {
        if (attempts_used != NULL) {
            *attempts_used = attempt;
        }
        (void)xEventGroupClearBits(s_events, WIFI_GOT_IP_BIT | WIFI_FAIL_BIT);

        esp_err_t err = esp_wifi_connect();
        if (err != ESP_OK) {
            ESP_LOGE(WIFI_MGR_TAG, "esp_wifi_connect failed: %s", esp_err_to_name(err));
        }

        const EventBits_t bits = xEventGroupWaitBits(
            s_events, WIFI_GOT_IP_BIT | WIFI_FAIL_BIT, pdFALSE, pdFALSE,
            pdMS_TO_TICKS(CONFIG_WSP_STA_CONNECT_TIMEOUT_MS));
        if ((bits & WIFI_GOT_IP_BIT) != 0) {
            connected = true;
            break;
        }
        ESP_LOGW(WIFI_MGR_TAG, "STA connect attempt %u/%u failed",
                 (unsigned)attempt, (unsigned)CONFIG_WSP_STA_MAX_RETRIES);
        vTaskDelay(pdMS_TO_TICKS(500));
    }
    return connected;
}

static esp_err_t start_softap(void)
{
    /* Use the station MAC for the SoftAP suffix as well, so the SSID suffix
     * matches the wserial-XXXXXX.local mDNS host name. */
    uint8_t mac[6] = {0};
    const esp_err_t mac_err = esp_read_mac(mac, ESP_MAC_WIFI_STA);
    if (mac_err != ESP_OK) {
        ESP_LOGW(WIFI_MGR_TAG, "cannot read SoftAP MAC: %s", esp_err_to_name(mac_err));
    }
    (void)snprintf(s_ap_ssid, sizeof(s_ap_ssid), "wserial-%02X%02X%02X",
                   mac[3], mac[4], mac[5]);

    wifi_config_t ap;
    memset(&ap, 0, sizeof(ap));
    (void)strlcpy((char *)ap.ap.ssid, s_ap_ssid, sizeof(ap.ap.ssid));
    ap.ap.ssid_len = (uint8_t)strlen(s_ap_ssid);
    const size_t pass_len = strlen(CONFIG_WSP_AP_PASSWORD);
    (void)strlcpy((char *)ap.ap.password, CONFIG_WSP_AP_PASSWORD, sizeof(ap.ap.password));
    ap.ap.channel = (uint8_t)CONFIG_WSP_AP_CHANNEL;
    ap.ap.max_connection = (uint8_t)CONFIG_WSP_AP_MAX_CLIENTS;
    ap.ap.authmode = (pass_len >= 8) ? WIFI_AUTH_WPA2_PSK : WIFI_AUTH_OPEN;
    ap.ap.pmf_cfg.required = false;

    (void)esp_wifi_stop();
    esp_err_t err = esp_wifi_set_mode(WIFI_MODE_APSTA);
    if (err != ESP_OK) {
        return err;
    }
    err = esp_wifi_set_config(WIFI_IF_AP, &ap);
    if (err != ESP_OK) {
        return err;
    }
    err = esp_wifi_start();
    if (err != ESP_OK) {
        return err;
    }

    char ip[16] = "192.168.4.1";
    esp_netif_ip_info_t ip_info;
    if (s_ap_netif != NULL && esp_netif_get_ip_info(s_ap_netif, &ip_info) == ESP_OK) {
        (void)esp_ip4addr_ntoa(&ip_info.ip, ip, sizeof(ip));
    }

    (void)xSemaphoreTake(s_lock, portMAX_DELAY);
    s_status.mode = WIFI_MGR_MODE_AP;
    s_status.connected = false;
    s_status.rssi = 0;
    (void)strlcpy(s_status.ip, ip, sizeof(s_status.ip));
    (void)strlcpy(s_status.ssid, s_ap_ssid, sizeof(s_status.ssid));
    (void)xSemaphoreGive(s_lock);

    ESP_LOGI(WIFI_MGR_TAG, "SoftAP \"%s\" up at http://%s/ (%s)",
             s_ap_ssid, ip, (pass_len >= 8) ? "WPA2" : "open");
    return ESP_OK;
}

esp_err_t wifi_mgr_init(void)
{
    s_lock = xSemaphoreCreateMutex();
    s_events = xEventGroupCreate();
    if (s_lock == NULL || s_events == NULL) {
        ESP_LOGE(WIFI_MGR_TAG, "cannot create synchronisation objects");
        return ESP_ERR_NO_MEM;
    }

    esp_err_t err = esp_netif_init();
    if (err != ESP_OK && err != ESP_ERR_INVALID_STATE) {
        ESP_LOGE(WIFI_MGR_TAG, "esp_netif_init failed: %s", esp_err_to_name(err));
        return err;
    }
    err = esp_event_loop_create_default();
    if (err != ESP_OK && err != ESP_ERR_INVALID_STATE) {
        ESP_LOGE(WIFI_MGR_TAG, "event loop init failed: %s", esp_err_to_name(err));
        return err;
    }

    s_sta_netif = esp_netif_create_default_wifi_sta();
    s_ap_netif = esp_netif_create_default_wifi_ap();
    if (s_sta_netif == NULL || s_ap_netif == NULL) {
        ESP_LOGE(WIFI_MGR_TAG, "cannot create network interfaces");
        return ESP_FAIL;
    }

    wifi_init_config_t init_cfg = WIFI_INIT_CONFIG_DEFAULT();
    err = esp_wifi_init(&init_cfg);
    if (err != ESP_OK) {
        ESP_LOGE(WIFI_MGR_TAG, "esp_wifi_init failed: %s", esp_err_to_name(err));
        return err;
    }
    err = esp_wifi_set_storage(WIFI_STORAGE_RAM);
    if (err != ESP_OK) {
        ESP_LOGE(WIFI_MGR_TAG, "esp_wifi_set_storage failed: %s", esp_err_to_name(err));
        return err;
    }
    err = esp_event_handler_instance_register(WIFI_EVENT, ESP_EVENT_ANY_ID,
                                              &on_network_event, NULL, NULL);
    if (err != ESP_OK) {
        return err;
    }
    err = esp_event_handler_instance_register(IP_EVENT, ESP_EVENT_ANY_ID,
                                              &on_network_event, NULL, NULL);
    if (err != ESP_OK) {
        return err;
    }

    /* No modem power save: the serial bridge cares about latency. */
    err = esp_wifi_set_ps(WIFI_PS_NONE);
    if (err != ESP_OK) {
        ESP_LOGW(WIFI_MGR_TAG, "esp_wifi_set_ps failed: %s", esp_err_to_name(err));
    }

    app_config_t cfg;
    config_store_get(&cfg);

    if (cfg.wifi_configured) {
        status_set_mode(WIFI_MGR_MODE_STA);
        err = start_station(&cfg);
        if (err == ESP_OK) {
            if (station_connect_with_retries(&s_status.retries)) {
                s_boot_phase = false;
                return ESP_OK;
            }
            ESP_LOGE(WIFI_MGR_TAG, "could not join \"%s\" after %u attempts, "
                                   "falling back to SoftAP",
                     cfg.wifi_ssid, (unsigned)CONFIG_WSP_STA_MAX_RETRIES);
        } else {
            ESP_LOGE(WIFI_MGR_TAG, "station start failed: %s, falling back to SoftAP",
                     esp_err_to_name(err));
        }
    } else {
        ESP_LOGW(WIFI_MGR_TAG, "no WiFi credentials, starting SoftAP provisioning mode");
    }

    /* Stay in boot phase: no background STA reconnects in provisioning mode. */
    s_boot_phase = true;
    err = start_softap();
    if (err != ESP_OK) {
        ESP_LOGE(WIFI_MGR_TAG, "SoftAP start failed: %s", esp_err_to_name(err));
        return err;
    }
    return ESP_OK;
}

void wifi_mgr_get_status(wifi_mgr_status_t *out)
{
    if (out == NULL) {
        return;
    }
    if (s_lock == NULL) {
        memset(out, 0, sizeof(*out));
        return;
    }
    (void)xSemaphoreTake(s_lock, portMAX_DELAY);
    *out = s_status;
    (void)xSemaphoreGive(s_lock);
}

esp_err_t wifi_mgr_get_mac(uint8_t mac[6])
{
    if (mac == NULL) {
        return ESP_ERR_INVALID_ARG;
    }
    return esp_read_mac(mac, ESP_MAC_WIFI_STA);
}

esp_err_t wifi_mgr_scan(wifi_ap_record_t *records, uint16_t max_records,
                        uint16_t *count)
{
    if (records == NULL || count == NULL || max_records == 0) {
        return ESP_ERR_INVALID_ARG;
    }
    *count = 0;

    wifi_scan_config_t scan_cfg;
    memset(&scan_cfg, 0, sizeof(scan_cfg));
    scan_cfg.show_hidden = true;

    esp_err_t err = esp_wifi_scan_start(&scan_cfg, true);
    if (err != ESP_OK) {
        ESP_LOGW(WIFI_MGR_TAG, "scan failed: %s", esp_err_to_name(err));
        return err;
    }

    uint16_t found = 0;
    err = esp_wifi_scan_get_ap_num(&found);
    if (err != ESP_OK) {
        (void)esp_wifi_clear_ap_list();
        return err;
    }

    uint16_t want = (found > max_records) ? max_records : found;
    if (want > 0) {
        err = esp_wifi_scan_get_ap_records(&want, records);
        if (err != ESP_OK) {
            (void)esp_wifi_clear_ap_list();
            return err;
        }
        *count = want;
    }
    (void)esp_wifi_clear_ap_list();
    return ESP_OK;
}
