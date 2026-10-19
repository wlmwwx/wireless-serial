/*
 * mdns_srv.c — mDNS advertisement using the espressif/mdns managed component.
 */

#include "mdns_srv.h"

#include <inttypes.h>
#include <stdio.h>
#include <string.h>

#include "esp_log.h"
#include "esp_mac.h"
#include "mdns.h"
#include "sdkconfig.h"

#include "config_store.h"

#define MDNS_SRV_TAG    "mdns_srv"
#define MDNS_SERVICE    "_wserial"
#define MDNS_PROTO      "_tcp"

static bool s_started;
static char s_hostname[32];
static char s_instance[64];

static esp_err_t add_service(const app_config_t *cfg)
{
    char baud[12];
    char raw[8];
    (void)snprintf(baud, sizeof(baud), "%" PRIu32, cfg->baud);
    (void)snprintf(raw, sizeof(raw), "%u", (unsigned)cfg->raw_port);

    mdns_txt_item_t txt[] = {
        { .key = "proto", .value = "1" },
        { .key = "raw", .value = raw },
        { .key = "name", .value = cfg->name },
        { .key = "baud", .value = baud },
    };
    return mdns_service_add(s_instance, MDNS_SERVICE, MDNS_PROTO, cfg->wsp_port,
                            txt, sizeof(txt) / sizeof(txt[0]));
}

esp_err_t mdns_srv_start(void)
{
    if (s_started) {
        return ESP_OK;
    }

    uint8_t mac[6] = {0};
    const esp_err_t mac_err = esp_read_mac(mac, ESP_MAC_WIFI_STA);
    if (mac_err != ESP_OK) {
        ESP_LOGW(MDNS_SRV_TAG, "cannot read MAC: %s", esp_err_to_name(mac_err));
    }
    (void)snprintf(s_hostname, sizeof(s_hostname), "wserial-%02X%02X%02X",
                   mac[3], mac[4], mac[5]);

    esp_err_t err = mdns_init();
    if (err != ESP_OK) {
        ESP_LOGE(MDNS_SRV_TAG, "mdns_init failed: %s", esp_err_to_name(err));
        return err;
    }
    err = mdns_hostname_set(s_hostname);
    if (err != ESP_OK) {
        ESP_LOGE(MDNS_SRV_TAG, "mdns_hostname_set failed: %s", esp_err_to_name(err));
        mdns_free();
        return err;
    }

    app_config_t cfg;
    config_store_get(&cfg);
    (void)strlcpy(s_instance, cfg.name, sizeof(s_instance));
    err = mdns_instance_name_set(s_instance);
    if (err != ESP_OK) {
        ESP_LOGW(MDNS_SRV_TAG, "mdns_instance_name_set failed: %s",
                 esp_err_to_name(err));
    }

    err = add_service(&cfg);
    if (err != ESP_OK) {
        ESP_LOGE(MDNS_SRV_TAG, "mdns_service_add failed: %s", esp_err_to_name(err));
        mdns_free();
        return err;
    }

    s_started = true;
    ESP_LOGI(MDNS_SRV_TAG, "%s.local -> %s._wserial._tcp port %u", s_hostname,
             s_instance, cfg.wsp_port);
    return ESP_OK;
}

esp_err_t mdns_srv_refresh(void)
{
    if (!s_started) {
        return ESP_ERR_INVALID_STATE;
    }

    app_config_t cfg;
    config_store_get(&cfg);

    /* Changing the instance name requires removing and re-adding the service;
     * plain TXT/port updates can be done in place. */
    if (strcmp(s_instance, cfg.name) != 0) {
        (void)mdns_service_remove(MDNS_SERVICE, MDNS_PROTO);
        (void)strlcpy(s_instance, cfg.name, sizeof(s_instance));
        (void)mdns_instance_name_set(s_instance);
        const esp_err_t err = add_service(&cfg);
        if (err != ESP_OK) {
            ESP_LOGW(MDNS_SRV_TAG, "cannot re-add mDNS service: %s",
                     esp_err_to_name(err));
        }
        return err;
    }

    char baud[12];
    char raw[8];
    (void)snprintf(baud, sizeof(baud), "%" PRIu32, cfg.baud);
    (void)snprintf(raw, sizeof(raw), "%u", (unsigned)cfg.raw_port);

    esp_err_t err = mdns_service_txt_item_set(MDNS_SERVICE, MDNS_PROTO, "baud", baud);
    if (err == ESP_OK) {
        err = mdns_service_txt_item_set(MDNS_SERVICE, MDNS_PROTO, "raw", raw);
    }
    if (err == ESP_OK) {
        err = mdns_service_txt_item_set(MDNS_SERVICE, MDNS_PROTO, "name", cfg.name);
    }
    if (err == ESP_OK) {
        err = mdns_service_port_set(MDNS_SERVICE, MDNS_PROTO, cfg.wsp_port);
    }
    if (err != ESP_OK) {
        ESP_LOGW(MDNS_SRV_TAG, "mDNS refresh failed: %s", esp_err_to_name(err));
    }
    return err;
}
