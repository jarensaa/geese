package main

import (
	"os"
	"regexp"
	"testing"
)

// The README and the getting-started guide show this example. They show it
// as it is here, where it compiles.
func TestDocumentsShowTheExample(t *testing.T) {
	block := regexp.MustCompile("(?ms)^```(go|json)\n(.*?)^```$")
	want := map[string]string{"go": read(t, "main.go"), "json": read(t, "flightplan.json")}
	for _, doc := range []string{"../../README.md", "../../docs/getting-started.md"} {
		shown := map[string]bool{}
		for _, m := range block.FindAllStringSubmatch(read(t, doc), -1) {
			lang, code := m[1], m[2]
			if code == want[lang] {
				shown[lang] = true
			}
		}
		for lang := range want {
			if !shown[lang] {
				t.Errorf("%s doesn't show the example's %s as it is in examples/quickstart", doc, lang)
			}
		}
	}
}

func read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
