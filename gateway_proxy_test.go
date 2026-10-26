package main

import (
	"strings"
	"testing"
)

// WithNoProxy and loopback are independent safeguards: the runtime regression
// exercises the shared dial options, while this holds every generated target,
// including plugin registration, to the local server's IPv4 loopback address.
func TestGatewaySelfDialTargetsAreLoopback(t *testing.T) {
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
			if got := strings.Count(rendered, `fmt.Sprintf("127.0.0.1:%d",`); got != tc.dials {
				t.Fatalf("loopback self-dials = %d, want %d", got, tc.dials)
			}
		})
	}
}
