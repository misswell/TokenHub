package main

import (
	"os"
	"strings"
	"testing"
)

// Upstream changes never cause a merge conflict in main.go. This guard instead
// makes startup drift visible so the additive entrypoint can be updated safely.
func TestDOPEntrypointPreservesUpstreamStartup(t *testing.T) {
	upstream, err := os.ReadFile("../tokenhub/main.go")
	if err != nil {
		t.Fatal(err)
	}
	custom, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	expected := strings.Replace(string(upstream), "app := server.NewWithConfig(store, config)\n", "app := server.NewWithConfig(store, config)\n\thandler, err := app.DOPBatchHandler()\n\tif err != nil {\n\t\tlog.Fatal(err)\n\t}\n", 1)
	expected = strings.Replace(expected, "Handler:           app.Handler(),", "Handler:           handler,", 1)
	expected = strings.Replace(expected, "log.Printf(\"tokenhub backend listening on %s\", addr)", "log.Printf(\"tokenhub backend with DOP batch API listening on %s\", addr)", 1)
	if string(custom) != expected {
		t.Fatal("upstream startup changed: synchronize cmd/tokenhub-dop/main.go while retaining the DOPBatchHandler opt-in")
	}
}
