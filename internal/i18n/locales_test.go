package i18n

import (
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

func loadCatalogue(t *testing.T, lang string) map[string]map[string]string {
	t.Helper()
	data, err := localesFS.ReadFile("locales/" + lang + ".yaml")
	if err != nil {
		t.Fatalf("read %s: %v", lang, err)
	}
	cat := map[string]map[string]string{}
	if err := yaml.Unmarshal(data, &cat); err != nil {
		t.Fatalf("parse %s: %v", lang, err)
	}
	return cat
}

func flatten(cat map[string]map[string]string) map[string]string {
	flat := map[string]string{}
	for section, entries := range cat {
		for key, value := range entries {
			flat[section+"."+key] = value
		}
	}
	return flat
}

// Every key must exist in both languages, otherwise a Turkish run silently prints the raw key.
func TestLocalesHaveSameKeys(t *testing.T) {
	en := flatten(loadCatalogue(t, "en"))
	tr := flatten(loadCatalogue(t, "tr"))

	var problems []string
	for key := range en {
		if _, ok := tr[key]; !ok {
			problems = append(problems, "missing from tr.yaml: "+key)
		}
	}
	for key := range tr {
		if _, ok := en[key]; !ok {
			problems = append(problems, "missing from en.yaml: "+key)
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

// Every catalogue key must be reachable through T, i.e. wired into the typed Locale lookup.
func TestEveryKeyResolvesThroughT(t *testing.T) {
	for _, lang := range []string{"en", "tr"} {
		if err := Init(lang); err != nil {
			t.Fatalf("Init(%s): %v", lang, err)
		}
		for key := range flatten(loadCatalogue(t, lang)) {
			if got := T(key); got == key {
				t.Errorf("%s: T(%q) returned the key itself; it is not wired into i18n.go", lang, key)
			}
		}
	}
	_ = Init("en")
}
