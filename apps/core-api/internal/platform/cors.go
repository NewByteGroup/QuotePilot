package platform

import "net/http"

// CORS is the browser-facing cross-origin layer mounted outermost on the
// mux, ahead of Correlation, CheckOrigin and Authenticate. It exists
// because the deployed web app lives on a sibling host (app.* calling
// api.* with cookie credentials): every cross-origin call first sends a
// preflight OPTIONS that matches no route, so without this layer the
// browser's question dies at the catch-all 404 before any real request
// is ever attempted.
//
// It is additive on top of CheckOrigin, never a replacement (ADR-005
// section 4): CORS tells the browser what it may send; the stdlib
// CrossOriginProtection still decides what the server accepts on
// state-changing routes. The allowlist is exactly the one trustedOrigin
// WebURL reduces to — no wildcard, and an untrusted Origin is passed
// through untouched rather than echoed, so a hostile origin cannot mint
// itself an allow by asking. Preflight answers a fixed method and header
// set — the contract's OrgHeader rides along because tenant-scoped reads
// carry it.
//
//	return platform.CORS(cfg)(mux)
func CORS(cfg *Config) func(http.Handler) http.Handler {
	trusted := trustedOrigin(cfg.WebURL)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != trusted {
				next.ServeHTTP(w, r)
				return
			}
			setAllowedOrigin(w.Header(), origin)
			if isPreflight(r) {
				answerPreflight(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// setAllowedOrigin stamps the response side of the trust decision: the
// request's own Origin value (already proven equal to the trusted one —
// never a reflected arbitrary string), credentials enabled because the
// qp_access cookie is the whole point of the cross-origin call, and Vary
// so a shared cache cannot serve this answer to a different origin.
func setAllowedOrigin(h http.Header, origin string) {
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Add("Vary", "Origin")
}

// isPreflight recognizes a real CORS preflight — an OPTIONS that declares
// the method it intends to send. An OPTIONS without it is an ordinary
// request and must still reach the mux.
func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
}

// answerPreflight responds 204 with the fixed allow set — every verb the
// contract surfaces use, Content-Type plus the tenant OrgHeader, and a
// two-hour max-age so an inbox session does not re-preflight every call.
// It never calls next: nothing downstream needs to see a permission
// question, and letting it through would only reach the catch-all 404.
func answerPreflight(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type, "+OrgHeader)
	h.Set("Access-Control-Max-Age", "7200")
	w.WriteHeader(http.StatusNoContent)
}
