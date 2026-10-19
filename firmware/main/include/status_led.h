/*
 * status_led.h — optional board LED activity indicator.
 */

#ifndef STATUS_LED_H
#define STATUS_LED_H

#include "esp_err.h"

/* No-op unless CONFIG_WSP_STATUS_LED is enabled. */
esp_err_t status_led_init(void);

#endif /* STATUS_LED_H */
