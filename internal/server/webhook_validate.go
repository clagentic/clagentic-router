// internal/server/webhook_validate.go — webhook input validation helpers.
//
// Centralises URL SSRF-prevention logic, the known-event allowlist, and the
// redacted list-response type. Nothing in this file makes network calls except
// validateWebhookURL's DNS lookup, which is intentional (registration-time check).
package server

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// blockedCIDRs is the list of IP ranges that webhook URLs must not resolve to.
// Covers loopback, link-local, RFC-1918 private, and IPv6 equivalents.
var blockedCIDRs = func() []*net.IPNet {
	blocks := []string{
		"127.0.0.0/8",     // loopback IPv4
		"::1/128",         // loopback IPv6
		"169.254.0.0/16",  // link-local IPv4
		"fe80::/10",       // link-local IPv6
		"10.0.0.0/8",      // RFC-1918
		"172.16.0.0/12",   // RFC-1918
		"192.168.0.0/16",  // RFC-1918
		"fc00::/7",        // unique local IPv6
		"0.0.0.0/8",       // this network
		"100.64.0.0/10",   // shared address (CGNAT)
		"192.0.2.0/24",    // TEST-NET-1
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved
	}
	nets := make([]*net.IPNet, 0, len(blocks))
	for _, b := range blocks {
		_, n, _ := net.ParseCIDR(b)
		nets = append(nets, n)
	}
	return nets
}()

// validateWebhookURL checks that rawURL is a safe, routable webhook target.
// Returns a user-facing error on failure, nil on success.
//
// Rules:
//   - Must parse as a valid URL.
//   - Scheme must be "https" (or "http" when allowHTTP is true — for dev).
//   - Host must not be empty.
//   - All resolved IPs must be outside blockedCIDRs.
//   - DNS resolution is performed at registration time. Re-validation at delivery
//     is the deliverer's responsibility.
func validateWebhookURL(rawURL string, allowHTTP bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && !(allowHTTP && scheme == "http") {
		return fmt.Errorf("webhook URL must use https scheme")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("webhook URL has no host")
	}

	// Reject bare IP literals in blocked ranges directly (no DNS needed).
	if ip := net.ParseIP(host); ip != nil {
		if err := checkIP(ip); err != nil {
			return fmt.Errorf("webhook URL: %w", err)
		}
		return nil
	}

	// DNS resolution — reject if any resolved address is in a blocked range.
	addrs, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("webhook URL: DNS resolution failed for %q: %w", host, err)
	}
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip == nil {
			continue
		}
		if err := checkIP(ip); err != nil {
			return fmt.Errorf("webhook URL: %w", err)
		}
	}
	return nil
}

func checkIP(ip net.IP) error {
	for _, cidr := range blockedCIDRs {
		if cidr.Contains(ip) {
			return fmt.Errorf("resolved address %s is in a blocked range (%s)", ip, cidr)
		}
	}
	return nil
}

// knownWebhookEvents is the set of valid event_type values that callers may
// subscribe to. These mirror the event strings emitted by the router alert hook
// (see internal/router/router.go stateChangeEvent and explicit Event: literals).
var knownWebhookEvents = map[string]struct{}{
	"offline":               {},
	"recovering":            {},
	"online":                {},
	"degraded":              {},
	"quota_low":             {},
	"quota_clear":           {},
	"auth_failure":          {},
	"rate_limited":          {},
	"state_change":          {},
	"quota_exhausted":       {},
	"backend_offline":       {},
	"backend_degraded":      {},
	"backend_recovered":     {},
	"backend_status_change": {},
}

// webhookListItem is the redacted view of a registered webhook returned by
// GET /webhooks. The Secret field is intentionally absent — callers only learn
// whether a secret is configured, not its value.
type webhookListItem struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	Events    string `json:"events"`
	HasSecret bool   `json:"has_secret"`
	CreatedAt string `json:"created_at"`
}
