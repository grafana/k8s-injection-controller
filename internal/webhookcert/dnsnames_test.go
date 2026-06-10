package webhookcert

import (
	"reflect"
	"testing"
)

func TestServiceDNSNames(t *testing.T) {
	tests := []struct {
		name      string
		addr      string
		wantDNS   string
		wantExtra []string
	}{
		{
			name:      "host with port",
			addr:      "svc-webhook.beyla.svc:443",
			wantDNS:   "svc-webhook.beyla.svc",
			wantExtra: []string{"svc-webhook.beyla.svc.cluster.local"},
		},
		{
			name:      "host without port",
			addr:      "svc-webhook.beyla.svc",
			wantDNS:   "svc-webhook.beyla.svc",
			wantExtra: []string{"svc-webhook.beyla.svc.cluster.local"},
		},
		{
			name:      "already cluster.local is not double-suffixed",
			addr:      "svc-webhook.beyla.svc.cluster.local:443",
			wantDNS:   "svc-webhook.beyla.svc.cluster.local",
			wantExtra: nil,
		},
		{
			name:      "empty",
			addr:      "",
			wantDNS:   "",
			wantExtra: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDNS, gotExtra := ServiceDNSNames(tt.addr)
			if gotDNS != tt.wantDNS {
				t.Errorf("dns = %q, want %q", gotDNS, tt.wantDNS)
			}
			if !reflect.DeepEqual(gotExtra, tt.wantExtra) {
				t.Errorf("extra = %#v, want %#v", gotExtra, tt.wantExtra)
			}
		})
	}
}
