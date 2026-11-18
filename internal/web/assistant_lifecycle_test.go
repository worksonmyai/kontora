package web

import "testing"

func TestAssistantLifecycleJS(t *testing.T) {
	for _, file := range []string{"assistant_lifecycle.test.mjs", "assistant_window.test.mjs"} {
		t.Run(file, func(t *testing.T) {
			if out, err := nodeTest(t, file).CombinedOutput(); err != nil {
				t.Fatalf("node assistant tests failed:\n%s", out)
			}
		})
	}
}
