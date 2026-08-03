package store

import "testing"

func TestNameKeyFoldsCase(t *testing.T) {
	cases := [][2]string{
		{"MixedCase.txt", "mixedcase.txt"},
		{"MIXEDCASE.TXT", "mixedcase.txt"},
		{"caf\u00e9.txt", "cafe\u0301.txt"},
		{"stra\u00dfe", "STRASSE"},
	}
	for _, c := range cases {
		a, b := nameKey(c[0]), nameKey(c[1])
		if a != b {
			t.Errorf("nameKey(%q)=%q != nameKey(%q)=%q", c[0], a, c[1], b)
		}
	}
	t.Logf("MixedCase -> %q", nameKey("MixedCase.txt"))
	t.Logf("MIXEDCASE -> %q", nameKey("MIXEDCASE.TXT"))
}
