/*
 * status_led.c — optional LED:
 *   solid on   : a serial client (WSP/RAW/WS) owns the UART
 *   slow blink : station mode, WiFi up
 *   fast blink : SoftAP provisioning mode
 *   medium     : connecting / no network yet
 */

#include "status_led.h"

#include "driver/gpio.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "sdkconfig.h"

#include "tcp_server.h"
#include "wifi_mgr.h"

#define STATUS_LED_TAG "status_led"

#ifdef CONFIG_WSP_STATUS_LED

#define STATUS_LED_PIN        ((gpio_num_t)CONFIG_WSP_STATUS_LED_GPIO)
#define STATUS_LED_TASK_STACK 2048
#define STATUS_LED_TASK_PRIO  2

static void led_write(bool on)
{
    const int level = CONFIG_WSP_STATUS_LED_ACTIVE_LOW ? (on ? 0 : 1) : (on ? 1 : 0);
    (void)gpio_set_level(STATUS_LED_PIN, level);
}

static void status_led_task(void *arg)
{
    (void)arg;
    bool on = false;
    TickType_t last_toggle = xTaskGetTickCount();

    for (;;) {
        const bool client = tcp_server_client_connected();
        if (client) {
            /* Solid on while a client is attached. */
            led_write(true);
            on = true;
            vTaskDelay(pdMS_TO_TICKS(100));
            continue;
        }

        wifi_mgr_status_t wifi;
        wifi_mgr_get_status(&wifi);
        uint32_t period_ms;
        if (wifi.mode == WIFI_MGR_MODE_AP) {
            period_ms = 250; /* fast blink: provisioning mode */
        } else if (wifi.connected) {
            period_ms = 1000; /* slow blink: idle station */
        } else {
            period_ms = 500; /* connecting */
        }

        if (xTaskGetTickCount() - last_toggle >= pdMS_TO_TICKS(period_ms)) {
            on = !on;
            led_write(on);
            last_toggle = xTaskGetTickCount();
        }
        vTaskDelay(pdMS_TO_TICKS(50));
    }
}

esp_err_t status_led_init(void)
{
    const gpio_config_t io = {
        .pin_bit_mask = (1ULL << (uint32_t)CONFIG_WSP_STATUS_LED_GPIO),
        .mode = GPIO_MODE_OUTPUT,
        .pull_up_en = GPIO_PULLUP_DISABLE,
        .pull_down_en = GPIO_PULLDOWN_DISABLE,
        .intr_type = GPIO_INTR_DISABLE,
    };
    esp_err_t err = gpio_config(&io);
    if (err != ESP_OK) {
        ESP_LOGE(STATUS_LED_TAG, "gpio_config failed: %s", esp_err_to_name(err));
        return err;
    }
    led_write(false);

    const BaseType_t ok = xTaskCreate(status_led_task, "status_led",
                                      STATUS_LED_TASK_STACK, NULL,
                                      STATUS_LED_TASK_PRIO, NULL);
    if (ok != pdPASS) {
        ESP_LOGE(STATUS_LED_TAG, "cannot create status LED task");
        return ESP_ERR_NO_MEM;
    }
    ESP_LOGI(STATUS_LED_TAG, "status LED on GPIO%d", CONFIG_WSP_STATUS_LED_GPIO);
    return ESP_OK;
}

#else /* !CONFIG_WSP_STATUS_LED */

esp_err_t status_led_init(void)
{
    return ESP_OK;
}

#endif /* CONFIG_WSP_STATUS_LED */
