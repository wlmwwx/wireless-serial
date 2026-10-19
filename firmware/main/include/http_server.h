/*
 * http_server.h — configuration UI, JSON API and WebSocket serial terminal.
 */

#ifndef HTTP_SERVER_H
#define HTTP_SERVER_H

#include "esp_err.h"

/* Start the HTTP server on CONFIG_WSP_HTTP_PORT (80 by default). */
esp_err_t http_server_start(void);

#endif /* HTTP_SERVER_H */
