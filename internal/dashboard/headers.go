package dashboard

import "net/http"

// contentSecurityPolicy limits what the dashboard's pages may load and do.
//
//   - frame-ancestors 'none': the dashboard can't be shown inside another
//     page. Without it, a page on another port of the same host (which the
//     browser treats as the same site, so the session cookie is sent) could
//     load it invisibly and trick a click on Stop or Upgrade (clickjacking).
//   - scripts, requests and forms only from the dashboard itself; the pages
//     use inline handlers (onclick=…), so inline script must be allowed, but
//     nothing can be loaded from elsewhere or sent anywhere else.
//   - Google Fonts is the one external source (CSS and font files only).
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline'; " +
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
	"font-src 'self' https://fonts.gstatic.com; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// securityHeaders adds browser security headers to every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Frame-Options", "DENY") // older browsers that ignore frame-ancestors
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}
