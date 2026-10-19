/*
 * mdns_srv.h — mDNS advertisement of the WSP service.
 *
 * Advertises _wserial._tcp with TXT records proto=1, raw=<port>,
 * name=<device>, baud=<current baud>, plus the host name
 * wserial-XXXXXX.local (last three MAC bytes).
 */

#ifndef MDNS_SRV_H
#define MDNS_SRV_H

#include "esp_err.h"

/* Start mDNS and register the service.  Call after the network is up. */
esp_err_t mdns_srv_start(void);

/* Re-publish name/baud/ports after a runtime configuration change. */
esp_err_t mdns_srv_refresh(void);

#endif /* MDNS_SRV_H */
