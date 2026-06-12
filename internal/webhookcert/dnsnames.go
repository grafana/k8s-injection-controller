// Package webhookcert holds helpers for the controller's self-managed webhook
// serving certificate (the cert-manager-less install path).
package webhookcert

import "strings"

// ServiceDNSNames derives the cert SANs for the webhook Service from the
// WEBHOOK_SERVICE_ADDR value the chart injects (host[:port]). It returns the
// primary DNS name (host, port stripped) and any extra SANs — currently the
// `.cluster.local` variant, matching the SANs the cert-manager Certificate
// issues today. An empty addr yields empty results.
func ServiceDNSNames(addr string) (dnsName string, extra []string) {
	if addr == "" {
		return "", nil
	}
	host := addr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	if strings.HasSuffix(host, ".svc") {
		extra = []string{host + ".cluster.local"}
	}
	return host, extra
}
