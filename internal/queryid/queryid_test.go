package queryid

import "testing"

func TestNewIsCanonicalAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		id := New()
		got, ok := Normalize(id)
		if !ok || got != id {
			t.Fatalf("New() = %q does not round-trip (%q, %v)", id, got, ok)
		}
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"q-7k3f9-xq2mp": "Q-7K3F9-XQ2MP",
		" Q7K3F9XQ2MP ": "Q-7K3F9-XQ2MP",
		"Q-7K3F9-XQ2MP": "Q-7K3F9-XQ2MP",
		"Q-OIL00-00000": "Q-01100-00000",
	}
	for in, want := range cases {
		if got, ok := Normalize(in); !ok || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "Q-", "Q-7K3F9", "Q-7K3F9-XQ2MPX", "Q-7K3F9-XQ2M!", "hello", "Q-UUUUU-UUUUU"} {
		if got, ok := Normalize(bad); ok {
			t.Errorf("Normalize(%q) accepted as %q", bad, got)
		}
	}
}
