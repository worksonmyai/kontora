package web

import "testing"

func TestResourceLifecycleJS(t *testing.T) {
	if out, err := nodeTest(t, "resource_lifecycle.test.mjs").CombinedOutput(); err != nil {
		t.Fatalf("node resource lifecycle tests failed:\n%s", out)
	}
}
