package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// ledProxy forwards /api/led and /api/led/* to ledd's HTTP API. When ledd
// does not answer, the caller gets 502 with a JSON error and the rest of the
// page keeps working.
func ledProxy(target string) (http.Handler, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	p := httputil.NewSingleHostReverseProxy(u)
	p.Transport = &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		ResponseHeaderTimeout: 2 * time.Second,
	}
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": "ledd: " + err.Error()})
	}
	return p, nil
}
