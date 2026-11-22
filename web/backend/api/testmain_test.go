package api_test

import (
	"os"
	"testing"

	"go.bison9.com/agency/internal/gateway"
)

func TestMain(m *testing.M) {
	gateway.RegisterToolProvidersForTest()
	os.Exit(m.Run())
}
