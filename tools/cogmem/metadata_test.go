package cogmem

import "testing"

func TestGlobalProvider_Metadata(t *testing.T) {
	if GlobalProvider.Namespace() != "cogmem" || GlobalProvider.Suite() != "cogmem" ||
		GlobalProvider.Description() != "Cognitive memory: durable domains and hooks" {
		t.Errorf("metadata: %q %q %q", GlobalProvider.Namespace(), GlobalProvider.Suite(), GlobalProvider.Description())
	}
	if ok, why := GlobalProvider.Available(nil); !ok || why != "" {
		t.Errorf("Available = %v %q", ok, why)
	}
}
