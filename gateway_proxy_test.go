package main

import (
	"strings"
	"testing"
)

// WithNoProxy and loopback are independent safeguards: the runtime regression
// exercises the shared dial options, while this holds every generated target,
// including plugin registration, to the address of the resolved local listener.
// The wire tests separately enforce loopback peers, including IPv6 and custom dialers.
func TestGatewaySelfDialTargetsUseResolvedListener(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dials int
	}{
		{"connect", 1},
		{"rest", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rendered := string(renderFactoryGo(t, "templates/factory/code/pkg/adapters/"+tc.name+"_gen.go.tmpl", &Settings{RestEndpoint: true, ConnectEndpoint: true}))
			if strings.Contains(rendered, `"0.0.0.0:%d"`) {
				t.Fatal("generated gateway still dials a wildcard address")
			}
			if got := strings.Count(rendered, `.rpcAddress()`); got != tc.dials {
				t.Fatalf("resolved self-dials = %d, want %d", got, tc.dials)
			}
		})
	}
}
