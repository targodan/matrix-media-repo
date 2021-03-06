package matrix

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/alioygur/is"
	"github.com/patrickmn/go-cache"
	"github.com/sirupsen/logrus"
	"github.com/turt2live/matrix-media-repo/common/config"
)

var apiUrlCacheInstance *cache.Cache
var apiUrlSingletonLock = &sync.Once{}

func setupCache() {
	if apiUrlCacheInstance == nil {
		apiUrlSingletonLock.Do(func() {
			apiUrlCacheInstance = cache.New(1*time.Hour, 2*time.Hour)
		})
	}
}

func GetServerApiUrl(hostname string) (string, string, error) {
	logrus.Info("Getting server API URL for " + hostname)

	// Check to see if we've cached this hostname at all
	setupCache()
	record, found := apiUrlCacheInstance.Get(hostname)
	if found {
		url := record.(string)
		logrus.Info("Server API URL for " + hostname + " is " + url + " (cache)")
		return url, hostname, nil
	}

	h, p, err := net.SplitHostPort(hostname)
	defPort := false
	if err != nil && strings.HasSuffix(err.Error(), "missing port in address") {
		h, p, err = net.SplitHostPort(hostname + ":8448")
		defPort = true
	}
	if err != nil {
		return "", "", err
	}

	// Step 1 of the discovery process: if the hostname is an IP, use that with explicit or default port
	logrus.Debug("Testing if " + h + " is an IP address")
	if is.IP(h) {
		url := fmt.Sprintf("https://%s:%s", h, p)
		apiUrlCacheInstance.Set(hostname, url, cache.DefaultExpiration)
		logrus.Info("Server API URL for " + hostname + " is " + url + " (IP address)")
		return url, hostname, nil
	}

	// Step 2: if the hostname is not an IP address, and an explicit port is given, use that
	logrus.Debug("Testing if a default port was used. Using default = ", defPort)
	if !defPort {
		url := fmt.Sprintf("https://%s:%s", h, p)
		apiUrlCacheInstance.Set(hostname, url, cache.DefaultExpiration)
		logrus.Info("Server API URL for " + hostname + " is " + url + " (explicit port)")
		return url, h, nil
	}

	// Step 3: if the hostname is not an IP address and no explicit port is given, do .well-known
	// Note that we have sprawling branches here because we need to fall through to step 4 if parsing fails
	logrus.Debug("Doing .well-known lookup on " + h)
	r, err := http.Get(fmt.Sprintf("https://%s/.well-known/matrix/server", h))
	if err == nil && r.StatusCode == http.StatusOK {
		// Try parsing .well-known
		c, err2 := ioutil.ReadAll(r.Body)
		if err2 == nil {
			wk := &wellknownServerResponse{}
			err3 := json.Unmarshal(c, wk)
			if err3 == nil && wk.ServerAddr != "" {
				wkHost, wkPort, err4 := net.SplitHostPort(wk.ServerAddr)
				wkDefPort := false
				if err4 != nil && strings.HasSuffix(err4.Error(), "missing port in address") {
					wkHost, wkPort, err4 = net.SplitHostPort(wk.ServerAddr + ":8448")
					wkDefPort = true
				}
				if err4 == nil {
					// Step 3a: if the delegated host is an IP address, use that (regardless of port)
					logrus.Debug("Checking if WK host is an IP: " + wkHost)
					if is.IP(wkHost) {
						url := fmt.Sprintf("https://%s:%s", wkHost, wkPort)
						apiUrlCacheInstance.Set(hostname, url, cache.DefaultExpiration)
						logrus.Info("Server API URL for " + hostname + " is " + url + " (WK; IP address)")
						return url, wk.ServerAddr, nil
					}

					// Step 3b: if the delegated host is not an IP and an explicit port is given, use that
					logrus.Debug("Checking if WK is using default port? ", wkDefPort)
					if !wkDefPort {
						url := fmt.Sprintf("https://%s:%s", wkHost, wkPort)
						apiUrlCacheInstance.Set(hostname, url, cache.DefaultExpiration)
						logrus.Info("Server API URL for " + hostname + " is " + url + " (WK; explicit port)")
						return url, wkHost, nil
					}

					// Step 3c: if the delegated host is not an IP and doesn't have a port, start a SRV lookup and use it
					// Note: we ignore errors here because the hostname will fail elsewhere.
					logrus.Debug("Doing SRV on WK host ", wkHost)
					_, addrs, _ := net.LookupSRV("matrix", "tcp", wkHost)
					if len(addrs) > 0 {
						// Trim off the trailing period if there is one (golang doesn't like this)
						realAddr := addrs[0].Target
						if realAddr[len(realAddr)-1:] == "." {
							realAddr = realAddr[0 : len(realAddr)-1]
						}
						url := fmt.Sprintf("https://%s:%d", realAddr, addrs[0].Port)
						apiUrlCacheInstance.Set(hostname, url, cache.DefaultExpiration)
						logrus.Info("Server API URL for " + hostname + " is " + url + " (WK; SRV)")
						return url, wkHost, nil
					}

					// Step 3d: use the delegated host as-is
					logrus.Debug("Using .well-known as-is for ", wkHost)
					url := fmt.Sprintf("https://%s:%s", wkHost, wkPort)
					apiUrlCacheInstance.Set(hostname, url, cache.DefaultExpiration)
					logrus.Info("Server API URL for " + hostname + " is " + url + " (WK; fallback)")
					return url, wkHost, nil
				}
			}
		}
	}

	// Step 4: try resolving a hostname using SRV records and use it
	// Note: we ignore errors here because the hostname will fail elsewhere.
	logrus.Debug("Doing SRV for host ", hostname)
	_, addrs, _ := net.LookupSRV("matrix", "tcp", hostname)
	if len(addrs) > 0 {
		// Trim off the trailing period if there is one (golang doesn't like this)
		realAddr := addrs[0].Target
		if realAddr[len(realAddr)-1:] == "." {
			realAddr = realAddr[0 : len(realAddr)-1]
		}
		url := fmt.Sprintf("https://%s:%d", realAddr, addrs[0].Port)
		apiUrlCacheInstance.Set(hostname, url, cache.DefaultExpiration)
		logrus.Info("Server API URL for " + hostname + " is " + url + " (SRV)")
		return url, h, nil
	}

	// Step 5: use the target host as-is
	logrus.Debug("Using host as-is: ", hostname)
	url := fmt.Sprintf("https://%s:%s", h, p)
	apiUrlCacheInstance.Set(hostname, url, cache.DefaultExpiration)
	logrus.Info("Server API URL for " + hostname + " is " + url + " (fallback)")
	return url, h, nil
}

func FederatedGet(url string, realHost string) (*http.Response, error) {
	// TODO: Support MSC1711 by relying on plain HTTPS requests to servers
	logrus.Info("Doing federated GET to " + url + " with host " + realHost)
	transport := &http.Transport{
		// Based on https://github.com/matrix-org/gomatrixserverlib/blob/51152a681e69a832efcd934b60080b92bc98b286/client.go#L74-L90
		DialTLS: func(network, addr string) (net.Conn, error) {
			dialer := &net.Dialer{
				Timeout: time.Duration(config.Get().TimeoutSeconds.Federation) * time.Second,
			}
			rawconn, err := dialer.Dial(network, addr)
			if err != nil {
				return nil, err
			}
			// Wrap a raw connection ourselves since tls.Dial defaults the SNI
			// #125: Some servers require SNI, so we should try it first. Most things on the planet support it.
			conn := tls.Client(rawconn, &tls.Config{
				ServerName:         realHost,
				InsecureSkipVerify: true,
			})
			if err := conn.Handshake(); err != nil {
				logrus.Warn("Handshake failed due to ", err, ". Attempting handshake without SNI.");
				// ...however there are reasons for some servers NOT supplying the correct ServerName, so fallback to not providing one.
				conn := tls.Client(rawconn, &tls.Config{
					ServerName:         "", // An empty ServerName means we will not try to verify it.
					InsecureSkipVerify: true,
				})
				if err := conn.Handshake(); err != nil {
					return nil, err;
				}
				return nil, err;
			}
			return conn, nil
		},
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	// Override the host to be compliant with the spec
	req.Header.Set("Host", realHost)
	req.Header.Set("User-Agent", "matrix-media-repo")
	req.Host = realHost

	resp, err := transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}
