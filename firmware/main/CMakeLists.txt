# Wireless Serial — component list for the firmware application.
#
# `espressif/mdns` is a managed component declared in main/idf_component.yml;
# the component manager adds it to this component's requirements automatically,
# so it does not appear in REQUIRES below.

idf_component_register(
    SRCS
        "app_main.c"
        "wsp_proto.c"
        "config_store.c"
        "uart_bridge.c"
        "wifi_mgr.c"
        "tcp_server.c"
        "http_server.c"
        "mdns_srv.c"
        "status_led.c"
    INCLUDE_DIRS
        "include"
    EMBED_TXTFILES
        "www/index.html"
    REQUIRES
        esp_wifi
        esp_netif
        esp_event
        esp_hw_support
        esp_system
        esp_driver_uart
        esp_driver_gpio
        esp_http_server
        esp_timer
        nvs_flash
        lwip
        json
)
