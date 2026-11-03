package core

import "testing"

func TestCanonRole(t *testing.T) {
	cases := map[string]string{
		"recon":         "Recon",
		"Recon":         "Recon",
		"web-exploiter": "Web-exploiter",
		"Main":          "Main",
		"":              "",
	}
	for in, want := range cases {
		if got := CanonRole(in); got != want {
			t.Errorf("CanonRole(%q) = %q, want %q", in, got, want)
		}
	}
}
