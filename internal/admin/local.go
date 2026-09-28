package admin

import (
	"net"
	"net/http"
	"net/url"
)

// LocalOnly rejects DNS rebinding hosts and cross-origin browser writes on
// the otherwise unauthenticated loopback management endpoint.
func LocalOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil || (host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback())) {
			writeError(w, http.StatusForbidden, "local host required")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || parsed.Scheme != "http" || parsed.Host != r.Host {
				writeError(w, http.StatusForbidden, "same origin required")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
