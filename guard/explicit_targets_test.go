package guard

import (
	"testing"

	"github.com/neouks/series/db"
)

func TestCollectTargetEndpoints(t *testing.T) {
	input := []byte(`{"target":"example.com","port":"8080","options":{"url":"https://1.1.1.1:9090/path"},"command":"curl http://192.0.2.44"}`)
	got := CollectTargetEndpoints(input)
	want := map[db.TargetEndpoint]bool{
		{Host: "example.com", Port: 8080}: true,
		{Host: "1.1.1.1", Port: 9090}:     true,
		{Host: "192.0.2.44", Port: 80}:    true,
	}
	if len(got) != len(want) {
		t.Fatalf("targets: %+v", got)
	}
	for _, endpoint := range got {
		if !want[endpoint] {
			t.Fatalf("unexpected target: %+v", endpoint)
		}
	}
}

func TestCollectTargetEndpointsChecksCommandAlongsideStructuredPort(t *testing.T) {
	input := []byte(`{"url":"https://example.com","port":8080,"command":"curl https://example.com"}`)
	got := CollectTargetEndpoints(input)
	want := map[int]bool{443: true, 8080: true}
	if len(got) != 2 {
		t.Fatalf("targets: %+v", got)
	}
	for _, endpoint := range got {
		if endpoint.Host != "example.com" || !want[endpoint.Port] {
			t.Fatalf("unexpected target: %+v", endpoint)
		}
	}
}
