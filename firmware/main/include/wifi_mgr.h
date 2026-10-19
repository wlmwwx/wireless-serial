/*
 * wifi_mgr.h — WiFi station with SoftAP fallback and network scanning.
 */

#ifndef WIFI_MGR_H
#define WIFI_MGR_H

#include <stdbool.h>
#include <stdint.h>

#include "esp_err.h"
#include "esp_wifi.h"

typedef enum {
    WIFI_MGR_MODE_NONE = 0,
    WIFI_MGR_MODE_STA, /* connected to an access point */
    WIFI_MGR_MODE_AP,  /* soft AP provisioning fallback */
} wifi_mgr_mode_t;

typedef struct {
    wifi_mgr_mode_t mode;
    bool     connected;  /* STA associated and got an IP */
    char     ip[16];     /* STA address or SoftAP gateway */
    char     ssid[33];   /* connected SSID or SoftAP SSID */
    int8_t   rssi;       /* STA only, 0 in AP mode */
    uint8_t  retries;    /* connect attempts used at boot */
} wifi_mgr_status_t;

/*
 * Initialise the WiFi stack and connect.
 *   - credentials available: up to CONFIG_WSP_STA_MAX_RETRIES attempts of
 *     CONFIG_WSP_STA_CONNECT_TIMEOUT_MS each;
 *   - no credentials or all attempts failed: start the wserial-XXXXXX SoftAP.
 * Blocks until either an IP is obtained or the fallback AP is up.
 */
esp_err_t wifi_mgr_init(void);

void wifi_mgr_get_status(wifi_mgr_status_t *out);

/* Factory MAC (used for mDNS host name and WSP device info). */
esp_err_t wifi_mgr_get_mac(uint8_t mac[6]);

/* Blocking WiFi scan.  Returns up to max_records access points. */
esp_err_t wifi_mgr_scan(wifi_ap_record_t *records, uint16_t max_records,
                        uint16_t *count);

#endif /* WIFI_MGR_H */
