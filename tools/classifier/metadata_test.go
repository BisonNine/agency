package classifier

import "testing"

func TestGlobalProvider_Metadata(t *testing.T) {
	if GlobalProvider.Namespace() != Namespace || GlobalProvider.Suite() != Suite ||
		GlobalProvider.Description() != "Classifiers: sort items from a tool with one model call each" {
		t.Errorf("metadata: %q %q %q", GlobalProvider.Namespace(), GlobalProvider.Suite(), GlobalProvider.Description())
	}
	if ok, why := GlobalProvider.Available(nil); !ok || why != "" {
		t.Errorf("Available = %v %q", ok, why)
	}
}
