/*
 * config_store.h — persistent device configuration (NVS backed).
 *
 * All runtime configuration (device name, UART format, TCP ports, WiFi
 * credentials) is kept in one snapshot.  Defaults come from Kconfig
 * (CONFIG_WSP_*) and are overridden by values found in NVS.
 */

#ifndef CONFIG_STORE_H
#define CONFIG_STORE_H

#include <stdbool.h>
#include <stdint.h>

#include "esp_err.h"

#define CONFIG_STORE_NAME_MAX 16  /* incl. NUL, WSP dev info field is 16 B   */
#define CONFIG_STORE_SSID_MAX 33  /* 802.11 SSID max 32 bytes + NUL        */
#define CONFIG_STORE_PASS_MAX 65  /* WPA2 passphrase max 64 bytes + NUL    */

typedef struct {
    char     name[CONFIG_STORE_NAME_MAX]; /* UTF-8, NUL padded            */
    uint32_t baud;
    uint8_t  data_bits;                   /* 5..8                        */
    uint8_t  parity;                      /* 0 none, 1 odd, 2 even       */
    uint8_t  stop_bits;                   /* 1..2                        */
    uint8_t  flow;                        /* 0 none, 1 RTS/CTS           */
    uint16_t wsp_port;
    uint16_t raw_port;
    char     wifi_ssid[CONFIG_STORE_SSID_MAX];
    char     wifi_pass[CONFIG_STORE_PASS_MAX];
    bool     wifi_configured;             /* ssid non-empty              */
} app_config_t;

/* Load defaults + NVS overrides.  Must be called once, after nvs_flash_init(). */
esp_err_t config_store_init(void);

/* Copy the current configuration snapshot into *out. */
void config_store_get(app_config_t *out);

/* Persist and apply a new UART format.  Returns ESP_ERR_INVALID_ARG when the
 * combination is out of range; NVS is only written for valid values. */
esp_err_t config_store_set_uart(uint32_t baud, uint8_t data_bits, uint8_t parity,
                                uint8_t stop_bits, uint8_t flow);

/* Persist a new device name (1..15 bytes, UTF-8). */
esp_err_t config_store_set_name(const char *name);

/* Persist new WSP / RAW TCP ports (takes effect after the next reboot). */
esp_err_t config_store_set_ports(uint16_t wsp_port, uint16_t raw_port);

/* Persist WiFi credentials.  An empty SSID clears the WiFi configuration.
 * A non-empty password shorter than 8 characters is rejected. */
esp_err_t config_store_set_wifi(const char *ssid, const char *pass);

#endif /* CONFIG_STORE_H */
